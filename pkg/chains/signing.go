/*
Copyright 2020 The Tekton Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package chains

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/go-multierror"
	"github.com/tektoncd/chains/pkg/artifacts"
	"github.com/tektoncd/chains/pkg/chains/formats"
	"github.com/tektoncd/chains/pkg/chains/formats/intotoite6"
	"github.com/tektoncd/chains/pkg/chains/formats/simple"
	"github.com/tektoncd/chains/pkg/chains/formats/tekton"
	"github.com/tektoncd/chains/pkg/chains/signing"
	"github.com/tektoncd/chains/pkg/chains/signing/kms"
	"github.com/tektoncd/chains/pkg/chains/signing/x509"
	"github.com/tektoncd/chains/pkg/chains/storage"
	"github.com/tektoncd/chains/pkg/config"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	versioned "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	"go.uber.org/zap"
	"knative.dev/pkg/logging"
)

type Signer interface {
	SignTaskRun(ctx context.Context, tr *v1beta1.TaskRun) error
}

type TaskRunSigner struct {
	// Formatters: format payload
	// The keys are the names of different formatters {tekton, in-toto, simplesigning}. The first two are for TaskRun artifact, and simplesigning is for OCI artifact.
	// The values are actual `Payloader` interfaces that can generate payload in different format from taskrun.
	Formatters map[formats.PayloadType]formats.Payloader

	// Backends: store payload and signature
	// The keys are different storage option's name. {docdb, gcs, grafeas, oci, tekton}
	// The values are the actual storage backends that will be used to store and retrieve provenance.
	Backends          map[string]storage.Backend
	SecretPath        string
	Pipelineclientset versioned.Interface
}

func allSigners(ctx context.Context, sp string, cfg config.Config, l *zap.SugaredLogger) map[string]signing.Signer {
	all := map[string]signing.Signer{}
	for _, s := range signing.AllSigners {
		switch s {
		case signing.TypeX509:
			signer, err := x509.NewSigner(ctx, sp, cfg, l)
			if err != nil {
				l.Warnf("error configuring x509 signer: %s", err)
				continue
			}
			all[s] = signer
		case signing.TypeKMS:
			signer, err := kms.NewSigner(ctx, cfg.Signers.KMS, l)
			if err != nil {
				l.Warnf("error configuring kms signer with config %v: %s", cfg.Signers.KMS, err)
				continue
			}
			all[s] = signer
		default:
			// This should never happen, so panic
			l.Panicf("unsupported signer: %s", s)
		}
	}
	return all
}

func AllFormatters(cfg config.Config, l *zap.SugaredLogger) map[formats.PayloadType]formats.Payloader {
	all := map[formats.PayloadType]formats.Payloader{}

	for _, f := range formats.AllFormatters {
		switch f {
		case formats.PayloadTypeTekton:
			formatter, err := tekton.NewFormatter()
			if err != nil {
				l.Warnf("error configuring tekton formatter: %s", err)
			}
			all[f] = formatter
		case formats.PayloadTypeSimpleSigning:
			formatter, err := simple.NewFormatter()
			if err != nil {
				l.Warnf("error configuring simplesigning formatter: %s", err)
			}
			all[f] = formatter
		case formats.PayloadTypeInTotoIte6:
			formatter, err := intotoite6.NewFormatter(cfg, l)
			if err != nil {
				l.Warnf("error configuring intoto formatter: %s", err)
			}
			all[f] = formatter
		}
	}

	return all
}

// SignTaskRun signs a TaskRun, and marks it as signed.
func (ts *TaskRunSigner) SignTaskRun(ctx context.Context, tr *v1beta1.TaskRun) error {
	cfg := *config.FromContext(ctx)
	logger := logging.FromContext(ctx)

	// TODO: Hook this up to config.
	enabledSignableTypes := []artifacts.Signable{
		&artifacts.TaskRunArtifact{Logger: logger},
		&artifacts.OCIArtifact{Logger: logger},
	}

	signers := allSigners(ctx, ts.SecretPath, cfg, logger)

	rekorClient, err := getRekor(cfg.Transparency.URL, logger)
	if err != nil {
		return err
	}

	var merr *multierror.Error
	extraAnnotations := map[string]string{}
	for _, signableType := range enabledSignableTypes {
		if !signableType.Enabled(cfg) {
			continue
		}
		payloadFormat := signableType.PayloadFormat(cfg)
		// Find the right payload format and format the object
		payloader, ok := ts.Formatters[payloadFormat]

		if !ok {
			logger.Warnf("Format %s configured for TaskRun: %v %s was not found", payloadFormat, tr, signableType.Type())
			continue
		}

		// Extract all the "things" to be signed.
		// We might have a few of each type (several binaries, or images)
		objects := signableType.ExtractObjects(tr)

		// Go through each object one at a time.
		for _, obj := range objects {

			payload, err := payloader.CreatePayload(obj)
			if err != nil {
				logger.Error(err)
				continue
			}
			logger.Infof("Created payload of type %s for TaskRun %s/%s", string(payloadFormat), tr.Namespace, tr.Name)

			// Sign it!
			signerType := signableType.Signer(cfg)
			signer, ok := signers[signerType]
			if !ok {
				logger.Warnf("No signer %s configured for %s", signerType, signableType.Type())
				continue
			}

			if payloader.Wrap() {
				wrapped, err := signing.Wrap(ctx, signer)
				if err != nil {
					return err
				}
				logger.Infof("Using wrapped envelope signer for %s", payloader.Type())
				signer = wrapped
			}

			logger.Infof("Signing object with %s", signerType)
			rawPayload, err := json.Marshal(payload)
			if err != nil {
				logger.Warnf("Unable to marshal payload: %v", signerType, obj)
				continue
			}

			signature, err := signer.SignMessage(bytes.NewReader(rawPayload))
			if err != nil {
				logger.Error(err)
				continue
			}

			// Now store those!
			for _, backend := range signableType.StorageBackend(cfg).List() {
				b := ts.Backends[backend]
				storageOpts := config.StorageOpts{
					Key:           signableType.Key(obj),
					Cert:          signer.Cert(),
					Chain:         signer.Chain(),
					PayloadFormat: payloadFormat,
				}
				if err := b.StorePayload(ctx, tr, rawPayload, string(signature), storageOpts); err != nil {
					logger.Error(err)
					merr = multierror.Append(merr, err)
				}
			}

			if shouldUploadTlog(cfg, tr) {
				entry, err := rekorClient.UploadTlog(ctx, signer, signature, rawPayload, signer.Cert(), string(payloadFormat))
				if err != nil {
					merr = multierror.Append(merr, err)
				} else {
					logger.Infof("Uploaded entry to %s with index %d", cfg.Transparency.URL, *entry.LogIndex)

					extraAnnotations[ChainsTransparencyAnnotation] = fmt.Sprintf("%s/api/v1/log/entries?logIndex=%d", cfg.Transparency.URL, *entry.LogIndex)
				}
			}
		}
		if merr.ErrorOrNil() != nil {
			if err := HandleRetry(ctx, tr, ts.Pipelineclientset, extraAnnotations); err != nil {
				merr = multierror.Append(merr, err)
			}
			return merr
		}
	}

	// Now mark the TaskRun as signed
	return MarkSigned(ctx, tr, ts.Pipelineclientset, extraAnnotations)
}

func HandleRetry(ctx context.Context, tr *v1beta1.TaskRun, ps versioned.Interface, annotations map[string]string) error {
	if RetryAvailable(tr) {
		return AddRetry(ctx, tr, ps, annotations)
	}
	return MarkFailed(ctx, tr, ps, annotations)
}
