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

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	externalsecrets "github.com/openshift/external-secrets-operator/pkg/controller/external_secrets"
	"github.com/openshift/external-secrets-operator/test/utils"
)

const (
	bitwardenSDKServerPodPrefix      = "bitwarden-sdk-server"
	bitwardenEgressNetworkPolicyName = "allow-egress-to-bitwarden-sdk-server"
)

// ensureBitwardenOperandReady enables the Bitwarden plugin on ExternalSecretsConfig,
// creates bitwarden-tls-certs (tls.crt, tls.key, ca.crt) in the operand namespace, sets secretRef,
// and waits for bitwarden-sdk-server to be reachable. Does not use bitwarden-creds (live cloud API token).
func ensureBitwardenOperandReady(ctx context.Context, tlsMaterials *utils.BitwardenTLSMaterials) error {
	clientset := suiteClientset
	dynamicClient := suiteDynamicClient
	runtimeClient := suiteRuntimeClient
	if clientset == nil || dynamicClient == nil || runtimeClient == nil {
		return fmt.Errorf("suite clients not initialized (run full suite or ensure BeforeSuite ran)")
	}

	var err error
	if tlsMaterials == nil {
		tlsMaterials, err = utils.GenerateSelfSignedCertForBitwardenServer()
		if err != nil {
			return err
		}
	}

	esc := &operatorv1alpha1.ExternalSecretsConfig{}
	if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc); err != nil {
		if k8serrors.IsNotFound(err) {
			createESC, err := loadExternalSecretsConfigFromFileWithBitwardenNetworkPolicy(testassets.ReadFile, externalSecretsFile)
			if err != nil {
				return err
			}
			if err := runtimeClient.Create(ctx, createESC); err != nil {
				return err
			}
			if err := utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute); err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		if _, err := ensureBitwardenEgressOnExternalSecretsConfig(ctx, runtimeClient); err != nil {
			return err
		}
		if err := utils.WaitForExternalSecretsConfigReady(ctx, dynamicClient, common.ExternalSecretsConfigObjectName, 2*time.Minute); err != nil {
			return err
		}
	}

	_, err = clientset.CoreV1().Namespaces().Get(ctx, utils.BitwardenOperandNamespace, metav1.GetOptions{})
	if err != nil && k8serrors.IsNotFound(err) {
		operandNS := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   utils.BitwardenOperandNamespace,
				Labels: map[string]string{"app": "external-secrets"},
			},
		}
		if _, err := clientset.CoreV1().Namespaces().Create(ctx, operandNS, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	_ = clientset.CoreV1().Secrets(utils.BitwardenOperandNamespace).Delete(ctx, utils.BitwardenTLSSecretName, metav1.DeleteOptions{})
	if err := utils.CreateBitwardenTLSSecret(ctx, clientset, utils.BitwardenOperandNamespace, utils.BitwardenTLSSecretName, tlsMaterials); err != nil {
		return err
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		esc := &operatorv1alpha1.ExternalSecretsConfig{}
		if err := runtimeClient.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc); err != nil {
			return err
		}
		if esc.Spec.Plugins.BitwardenSecretManagerProvider == nil {
			esc.Spec.Plugins.BitwardenSecretManagerProvider = &operatorv1alpha1.BitwardenSecretManagerProvider{}
		}
		esc.Spec.Plugins.BitwardenSecretManagerProvider.Mode = operatorv1alpha1.Enabled
		esc.Spec.Plugins.BitwardenSecretManagerProvider.SecretRef = &operatorv1alpha1.SecretReference{Name: utils.BitwardenTLSSecretName}
		return runtimeClient.Update(ctx, esc)
	})
	if err != nil {
		return err
	}

	if err := utils.RestartBitwardenSDKServerPods(ctx, clientset); err != nil {
		return err
	}
	if err := utils.VerifyPodsReadyByPrefix(ctx, clientset, utils.BitwardenOperandNamespace, []string{bitwardenSDKServerPodPrefix}); err != nil {
		return err
	}
	if err := utils.VerifyPodsReadyByPrefix(ctx, clientset, utils.BitwardenOperandNamespace, []string{externalsecrets.OperandWebhookPodPrefix}); err != nil {
		return err
	}
	if err := utils.WaitForBitwardenSDKServerReachableFromCluster(ctx, clientset, 90*time.Second); err != nil {
		return err
	}
	return nil
}

// loadExternalSecretsConfigFromFileWithBitwardenNetworkPolicy loads the cluster ExternalSecretsConfig from the
// given file and appends the network policy that allows the main controller to reach bitwarden-sdk-server on port 9998.
func loadExternalSecretsConfigFromFileWithBitwardenNetworkPolicy(assetFunc func(string) ([]byte, error), filename string) (*operatorv1alpha1.ExternalSecretsConfig, error) {
	data, err := assetFunc(filename)
	if err != nil {
		return nil, err
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 1024)
	var rawObj runtime.RawExtension
	if err := decoder.Decode(&rawObj); err != nil {
		return nil, err
	}
	obj, _, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(rawObj.Raw, nil, nil)
	if err != nil {
		return nil, err
	}
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	esc := &operatorv1alpha1.ExternalSecretsConfig{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredMap, esc); err != nil {
		return nil, err
	}
	esc.Spec.ControllerConfig.NetworkPolicies = append(esc.Spec.ControllerConfig.NetworkPolicies, bitwardenEgressNetworkPolicy())
	return esc, nil
}

func bitwardenEgressNetworkPolicy() operatorv1alpha1.NetworkPolicy {
	port9998 := intstr.FromInt32(9998)
	tcp := corev1.ProtocolTCP
	return operatorv1alpha1.NetworkPolicy{
		Name:          bitwardenEgressNetworkPolicyName,
		ComponentName: operatorv1alpha1.CoreController,
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "bitwarden-sdk-server"}}},
				},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &port9998},
				},
			},
		},
	}
}

func ensureBitwardenEgressOnExternalSecretsConfig(ctx context.Context, c client.Client) (bool, error) {
	var updated bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		esc := &operatorv1alpha1.ExternalSecretsConfig{}
		if err := c.Get(ctx, client.ObjectKey{Name: common.ExternalSecretsConfigObjectName}, esc); err != nil {
			return err
		}
		for _, np := range esc.Spec.ControllerConfig.NetworkPolicies {
			if np.Name == bitwardenEgressNetworkPolicyName {
				return nil
			}
		}
		esc.Spec.ControllerConfig.NetworkPolicies = append(esc.Spec.ControllerConfig.NetworkPolicies, bitwardenEgressNetworkPolicy())
		if err := c.Update(ctx, esc); err != nil {
			return err
		}
		updated = true
		return nil
	})
	return updated, err
}
