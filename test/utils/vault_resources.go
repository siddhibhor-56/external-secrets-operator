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
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	vaultImage    = "docker.io/hashicorp/vault:1.17.5"
	vaultPort     = 8200
	vaultDevToken = "root"
	vaultPodName  = "vault-dev"
	vaultSvcName  = "vault-dev-svc"
	vaultLabelKey = "app"
	vaultLabelVal = "vault-dev"
)

// VaultSecretStore returns a namespaced SecretStore for Vault KV v2.
func VaultSecretStore(name, namespace, vaultURL, tokenSecretName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": externalSecretsAPIVersionV1,
			"kind":       secretStoreKind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "external-secrets-operator-e2e",
				},
			},
			"spec": map[string]interface{}{
				"provider": map[string]interface{}{
					"vault": map[string]interface{}{
						"server":  vaultURL,
						"path":    "secret",
						"version": "v2",
						"auth": map[string]interface{}{
							"tokenSecretRef": map[string]interface{}{
								"name": tokenSecretName,
								"key":  "token",
							},
						},
					},
				},
			},
		},
	}
}

// DeployVaultDevServer creates a Vault dev-mode Pod and Service in the given namespace.
// Returns the pod name, the in-cluster Vault URL, a cleanup function, and any error.
func DeployVaultDevServer(ctx context.Context, clientset kubernetes.Interface, namespace string) (string, string, func(), error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vaultPodName,
			Namespace: namespace,
			Labels: map[string]string{
				vaultLabelKey: vaultLabelVal,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "vault",
					Image: vaultImage,
					Env: []corev1.EnvVar{
						{Name: "VAULT_DEV_ROOT_TOKEN_ID", Value: vaultDevToken},
						{Name: "VAULT_DEV_LISTEN_ADDRESS", Value: "0.0.0.0:8200"},
						{Name: "VAULT_ADDR", Value: "http://127.0.0.1:8200"},
						{Name: "SKIP_SETCAP", Value: "1"},
						{Name: "HOME", Value: "/tmp"},
					},
					Ports: []corev1.ContainerPort{
						{ContainerPort: int32(vaultPort), Protocol: corev1.ProtocolTCP},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/v1/sys/health",
								Port:   intstr.FromInt(vaultPort),
								Scheme: corev1.URISchemeHTTP,
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       5,
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: falsePtr(),
						RunAsNonRoot:             truePtr(),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
				},
			},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vaultSvcName,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				vaultLabelKey: vaultLabelVal,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       int32(vaultPort),
					TargetPort: intstr.FromInt(vaultPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	_, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", "", nil, fmt.Errorf("create vault pod: %w", err)
	}

	_, err = clientset.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		return "", "", nil, fmt.Errorf("create vault service: %w", err)
	}

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := clientset.CoreV1().Pods(namespace).Get(ctx, vaultPodName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return p.Status.Phase == corev1.PodRunning && isPodReady(p), nil
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("wait for vault pod ready: %w", err)
	}

	vaultURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", vaultSvcName, namespace, vaultPort)
	cleanup := func() {
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), vaultPodName, metav1.DeleteOptions{})
		_ = clientset.CoreV1().Services(namespace).Delete(context.Background(), vaultSvcName, metav1.DeleteOptions{})
	}

	return vaultPodName, vaultURL, cleanup, nil
}

// CreateVaultTokenSecret creates a k8s Secret holding the Vault dev token.
func CreateVaultTokenSecret(ctx context.Context, clientset kubernetes.Interface, namespace, secretName string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		StringData: map[string]string{
			"token": vaultDevToken,
		},
	}
	_, err := clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	return err
}

// ExecInPod runs a command inside a pod and returns stdout.
func ExecInPod(ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface, namespace, podName string, command []string) (string, error) {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdout:  true,
			Stderr:  true,
		}, clientscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return "", fmt.Errorf("exec command %v: %w (stderr: %s)", command, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// WriteVaultKVSecret writes a key-value pair to Vault KV v2 at secret/<secretPath>.
func WriteVaultKVSecret(ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface, namespace, podName, secretPath, key, value string) error {
	cmd := []string{"vault", "kv", "put", fmt.Sprintf("secret/%s", secretPath), fmt.Sprintf("%s=%s", key, value)}
	_, err := ExecInPod(ctx, cfg, clientset, namespace, podName, cmd)
	return err
}

// ReadVaultKVSecret reads a field from Vault KV v2 at secret/<secretPath>.
func ReadVaultKVSecret(ctx context.Context, cfg *rest.Config, clientset kubernetes.Interface, namespace, podName, secretPath, key string) (string, error) {
	cmd := []string{"vault", "kv", "get", "-field=" + key, fmt.Sprintf("secret/%s", secretPath)}
	return ExecInPod(ctx, cfg, clientset, namespace, podName, cmd)
}

func truePtr() *bool  { t := true; return &t }
func falsePtr() *bool { f := false; return &f }
