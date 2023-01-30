// Copyright 2022 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/hacbs-contract/enterprise-contract-controller/api/v1alpha1"
	"github.com/in-toto/in-toto-golang/in_toto"
	v02 "github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/v0.2"
	conftestOutput "github.com/open-policy-agent/conftest/output"
	"github.com/sigstore/cosign/pkg/cosign"
	"github.com/sigstore/cosign/pkg/oci"
	"github.com/sigstore/cosign/pkg/oci/static"
	cosignTypes "github.com/sigstore/cosign/pkg/types"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"

	"github.com/hacbs-contract/ec-cli/internal/evaluation_target/application_snapshot_image"
	"github.com/hacbs-contract/ec-cli/internal/policy"
)

const (
	imageRegistry = "registry.example/spam"
	imageTag      = "maps"
	imageDigest   = "4e388ab32b10dc8dbc7e28144f552830adc74787c1e2c0824032078a79f227fb"
	imageRef      = imageRegistry + ":" + imageTag + "@sha256:" + imageDigest
)

func TestValidateImage(t *testing.T) {
	cases := []struct {
		name               string
		client             *mockASIClient
		url                string
		expectedViolations []conftestOutput.Result
		expectedWarnings   []conftestOutput.Result
		expectedImageURL   string
	}{
		{
			name: "simple success",
			client: &mockASIClient{
				head:         &gcr.Descriptor{},
				signatures:   []oci.Signature{validSignature},
				attestations: []oci.Signature{validAttestation},
			},
			url:                imageRef,
			expectedViolations: []conftestOutput.Result{},
			expectedWarnings:   []conftestOutput.Result{},
			expectedImageURL:   imageRegistry + "@sha256:" + imageDigest,
		},
		{
			name:   "unaccessible image",
			client: &mockASIClient{},
			url:    imageRef,
			expectedViolations: []conftestOutput.Result{
				{Message: "image ref not accessible. no response received"},
			},
			expectedWarnings: []conftestOutput.Result{},
			expectedImageURL: imageRef,
		},
		{
			name: "no image signatures",
			client: &mockASIClient{
				head:         &gcr.Descriptor{},
				attestations: []oci.Signature{validAttestation},
			},
			url: imageRef,
			expectedViolations: []conftestOutput.Result{
				{Message: "no image signatures client error"},
			},
			expectedWarnings: []conftestOutput.Result{},
			expectedImageURL: imageRegistry + "@sha256:" + imageDigest,
		},
		{
			name: "no image attestations",
			client: &mockASIClient{
				head:       &gcr.Descriptor{},
				signatures: []oci.Signature{validSignature},
			},
			url: imageRef,
			expectedViolations: []conftestOutput.Result{
				{Message: "no image attestations client error"},
				{Message: "EV001: No attestation data, at ..."},
				{Message: "no attestations available"},
			},
			expectedWarnings: []conftestOutput.Result{},
			expectedImageURL: imageRegistry + "@sha256:" + imageDigest,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := afero.NewMemMapFs()

			p := &policy.Policy{
				EnterpriseContractPolicySpec: v1alpha1.EnterpriseContractPolicySpec{},
				CheckOpts:                    &cosign.CheckOpts{},
				EffectiveTime:                time.Now().UTC(),
			}

			ctx := context.Background()
			ctx = application_snapshot_image.WithClient(ctx, c.client)

			actual, err := ValidateImage(ctx, fs, c.url, p)
			assert.NoError(t, err)

			assert.Equal(t, c.expectedWarnings, actual.Warnings())
			actualViolations := removePathFromErrors(actual.Violations())
			assert.Equal(t, c.expectedViolations, actualViolations)
			assert.Equal(t, c.expectedImageURL, actual.ImageURL)
		})
	}
}

var errorPathRegex = regexp.MustCompile(`(.*, at) (.*)$`)

// removePathFromErrors replaces literal code paths with "...". Such paths may appear
// in certain violation messages. This function normalizes the value to facilitate
// matching in tests.
func removePathFromErrors(results []conftestOutput.Result) []conftestOutput.Result {
	newResults := make([]conftestOutput.Result, 0, len(results))
	for _, result := range results {
		result.Message = errorPathRegex.ReplaceAllString(result.Message, "$1 ...")
		newResults = append(newResults, result)
	}
	return newResults
}

type mockASIClient struct {
	head         *gcr.Descriptor
	signatures   []oci.Signature
	attestations []oci.Signature
}

func (c *mockASIClient) VerifyImageSignatures(ctx context.Context, ref name.Reference, opts *cosign.CheckOpts) ([]oci.Signature, bool, error) {
	if len(c.signatures) == 0 {
		return nil, false, errors.New("no image signatures client error")
	}
	return c.signatures, false, nil
}

func (c *mockASIClient) VerifyImageAttestations(ctx context.Context, ref name.Reference, opts *cosign.CheckOpts) ([]oci.Signature, bool, error) {
	if len(c.attestations) == 0 {
		return nil, false, errors.New("no image attestations client error")
	}
	return c.attestations, false, nil
}

func (c *mockASIClient) Head(ref name.Reference, opts ...remote.Option) (*gcr.Descriptor, error) {
	return c.head, nil
}

func sign(statement *in_toto.Statement) oci.Signature {
	statementJson, err := json.Marshal(statement)
	if err != nil {
		panic(err)
	}
	payload := base64.StdEncoding.EncodeToString(statementJson)
	signature, err := static.NewSignature(
		[]byte(`{"payload":"`+payload+`"}`),
		"signature",
		static.WithLayerMediaType(types.MediaType((cosignTypes.DssePayloadType))),
	)
	if err != nil {
		panic(err)
	}
	return signature
}

var validSignature = sign(&in_toto.Statement{
	StatementHeader: in_toto.StatementHeader{
		Type:          in_toto.StatementInTotoV01,
		PredicateType: v02.PredicateSLSAProvenance,
		Subject: []in_toto.Subject{
			{Name: imageRegistry, Digest: v02.DigestSet{"sha256": imageDigest}},
		},
	},
})

var validAttestation = sign(&in_toto.Statement{
	StatementHeader: in_toto.StatementHeader{
		Type:          in_toto.StatementInTotoV01,
		PredicateType: v02.PredicateSLSAProvenance,
		Subject: []in_toto.Subject{
			{Name: imageRegistry, Digest: v02.DigestSet{"sha256": imageDigest}},
		},
	},
	Predicate: v02.ProvenancePredicate{
		BuildType: "https://tekton.dev/attestations/chains/pipelinerun@v2",
		Builder: v02.ProvenanceBuilder{
			ID: "scheme:uri",
		},
	},
})
