//go:build e2e
// +build e2e

/*
Copyright 2025.

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

package utils

import (
	"context"

	"k8s.io/client-go/kubernetes"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	externalsecrets "github.com/openshift/external-secrets-operator/pkg/controller/external_secrets"
)

// IsCertManagerConfigEnabled reports whether ExternalSecretsConfig is configured to use cert-manager
// instead of the in-tree cert-controller operand.
func IsCertManagerConfigEnabled(esc *operatorv1alpha1.ExternalSecretsConfig) bool {
	if esc == nil || esc.Spec.ControllerConfig.CertProvider == nil ||
		esc.Spec.ControllerConfig.CertProvider.CertManager == nil {
		return false
	}
	return common.EvalMode(esc.Spec.ControllerConfig.CertProvider.CertManager.Mode)
}

// IsCertControllerExpected reports whether the cert-controller deployment should exist for the given config.
func IsCertControllerExpected(esc *operatorv1alpha1.ExternalSecretsConfig) bool {
	return !IsCertManagerConfigEnabled(esc)
}

// OperandPodPrefixes returns operand pod name prefixes that must be ready for the given config.
func OperandPodPrefixes(esc *operatorv1alpha1.ExternalSecretsConfig) []string {
	prefixes := []string{
		externalsecrets.OperandCoreControllerPodPrefix,
		externalsecrets.OperandWebhookPodPrefix,
	}
	if IsCertControllerExpected(esc) {
		prefixes = append(prefixes, externalsecrets.OperandCertControllerPodPrefix)
	}
	return prefixes
}

// VerifyOperandPodsReady waits for operand pods required by the given ExternalSecretsConfig to be ready.
func VerifyOperandPodsReady(ctx context.Context, clientset kubernetes.Interface, namespace string, esc *operatorv1alpha1.ExternalSecretsConfig) error {
	return VerifyPodsReadyByPrefix(ctx, clientset, namespace, OperandPodPrefixes(esc))
}
