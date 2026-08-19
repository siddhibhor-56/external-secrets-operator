//go:build e2e
// +build e2e

/*
Copyright 2026.
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
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/remotecommand"
)

// PodExecOptions contains options for executing commands in a pod
type PodExecOptions struct {
	Namespace     string
	PodName       string
	ContainerName string // Optional: leave empty for default container
	Command       []string
	Stdin         io.Reader // Optional: for interactive commands
}

func NewClientsConfigForTest(t *testing.T) (kubernetes.Interface, dynamic.Interface) {
	config, err := GetConfigForTest(t)
	if err == nil {
		t.Logf("Found configuration for host %v.\n", config.Host)
	}

	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	dynamicKubeConfig, err := dynamic.NewForConfig(config)
	require.NoError(t, err)
	return kubeClient, dynamicKubeConfig
}

func GetConfigForTest(t *testing.T) (*rest.Config, error) {
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{ClusterInfo: api.Cluster{InsecureSkipTLSVerify: true}})
	config, err := clientConfig.ClientConfig()
	if err == nil {
		t.Logf("Found configuration for host %v.\n", config.Host)
	}

	require.NoError(t, err)
	return config, err
}

// ExecCommandInPod executes a command in a pod using client-go's remotecommand
// Returns stdout, stderr, and error
func ExecCommandInPod(ctx context.Context, client kubernetes.Interface, config *rest.Config, opts PodExecOptions) (string, string, error) {
	if len(opts.Command) == 0 {
		return "", "", fmt.Errorf("command cannot be empty")
	}

	// Prepare the API request
	req := client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(opts.PodName).
		Namespace(opts.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: opts.ContainerName,
			Command:   opts.Command,
			Stdin:     opts.Stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, kubescheme.ParameterCodec)

	// Create the executor
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	// Prepare buffers for stdout and stderr
	var stdout, stderr bytes.Buffer

	// Execute the command
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  opts.Stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	return stdout.String(), stderr.String(), err
}

// GetOperatorPodNodeArchitecture returns the architecture of the node where a pod matching the label selector is running
// This is useful for ensuring test pods (like Vault) are scheduled on compatible architecture nodes in multi-arch clusters
func GetOperatorPodNodeArchitecture(ctx context.Context, clientset *kubernetes.Clientset, namespace, labelSelector string) (string, error) {
	// List pods with label selector
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods with selector %s: %w", labelSelector, err)
	}

	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found with selector: %s in namespace: %s", labelSelector, namespace)
	}

	// Get the node name where the first running pod is scheduled
	var nodeName string
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.Spec.NodeName != "" {
			nodeName = pod.Spec.NodeName
			break
		}
	}

	if nodeName == "" {
		return "", fmt.Errorf("no running pod found with selector: %s in namespace: %s", labelSelector, namespace)
	}

	// Get node details
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	// Return the architecture
	arch := node.Status.NodeInfo.Architecture
	return arch, nil
}

// ApplyManifestFromReader applies Kubernetes manifests from a reader function using dynamic client
// This supports embedded files via embed.FS
func ApplyManifestFromReader(ctx context.Context, dynamicClient dynamic.Interface, readFunc func(string) ([]byte, error), filePath string) error {
	// Read the file using the provided function
	data, err := readFunc(filePath)
	if err != nil {
		return fmt.Errorf("failed to read manifest file: %w", err)
	}

	return ApplyManifest(ctx, dynamicClient, data)
}

// ApplyManifestFromReaderWithReplacements applies Kubernetes manifests from a reader function
// with string replacements for template variables
func ApplyManifestFromReaderWithReplacements(ctx context.Context, dynamicClient dynamic.Interface, readFunc func(string) ([]byte, error), filePath string, replacements map[string]string) error {
	// Read the file using the provided function
	data, err := readFunc(filePath)
	if err != nil {
		return fmt.Errorf("failed to read manifest file: %w", err)
	}

	// Apply replacements
	content := string(data)
	for placeholder, value := range replacements {
		content = strings.ReplaceAll(content, placeholder, value)
	}

	return ApplyManifest(ctx, dynamicClient, []byte(content))
}

// ApplyManifest applies Kubernetes manifests from byte data using dynamic client
func ApplyManifest(ctx context.Context, dynamicClient dynamic.Interface, data []byte) error {

	// Split YAML documents
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to decode manifest: %w", err)
		}

		// Skip empty documents
		if obj.Object == nil {
			continue
		}

		// Get GVR (GroupVersionResource) from the object
		gvk := obj.GroupVersionKind()
		gvr := schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: pluralizeResource(gvk.Kind),
		}

		namespace := obj.GetNamespace()
		name := obj.GetName()

		// Try to get the resource first
		var resourceClient dynamic.ResourceInterface
		if namespace != "" {
			resourceClient = dynamicClient.Resource(gvr).Namespace(namespace)
		} else {
			resourceClient = dynamicClient.Resource(gvr)
		}

		existing, err := resourceClient.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			// Resource exists, update it
			obj.SetResourceVersion(existing.GetResourceVersion())
			_, err = resourceClient.Update(ctx, &obj, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update %s/%s: %w", gvk.Kind, name, err)
			}
		} else {
			// Resource doesn't exist, create it
			_, err = resourceClient.Create(ctx, &obj, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create %s/%s: %w", gvk.Kind, name, err)
			}
		}
	}

	return nil
}

// pluralizeResource converts Kind to resource name (simple pluralization)
// For production use, consider using discovery client or a proper pluralization library
func pluralizeResource(kind string) string {
	// Convert to lowercase first
	lower := strings.ToLower(kind)

	// Simple pluralization rules
	switch lower {
	case "endpoints":
		return "endpoints"
	case "networkpolicy":
		return "networkpolicies"
	case "ingress":
		return "ingresses"
	case "policy":
		return "policies"
	default:
		// Simple rule: add 's' to lowercase kind
		return lower + "s"
	}
}

// GetClusterArchitecture detects the architecture of the Kubernetes cluster
// by checking all worker nodes and returning the most common architecture
// For multi-arch clusters, it prioritizes non-amd64 architectures (ppc64le, arm64, s390x)
func GetClusterArchitecture(ctx context.Context, client kubernetes.Interface) (string, error) {
	// List all nodes
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes found in cluster")
	}

	// Count architectures across all nodes
	archCount := make(map[string]int)

	for _, node := range nodes.Items {
		var arch string

		// Try multiple sources for architecture information
		if a, ok := node.Labels["kubernetes.io/arch"]; ok && a != "" {
			arch = a
		} else if a, ok := node.Labels["beta.kubernetes.io/arch"]; ok && a != "" {
			arch = a
		} else if node.Status.NodeInfo.Architecture != "" {
			arch = node.Status.NodeInfo.Architecture
		}

		if arch != "" {
			archCount[arch]++
		}
	}

	if len(archCount) == 0 {
		return "", fmt.Errorf("no architecture information found on any node")
	}

	// For multi-arch clusters, prioritize non-amd64 architectures
	// This is because tests are often run on specialized architectures
	priorityArchs := []string{"ppc64le", "s390x", "arm64"}
	for _, arch := range priorityArchs {
		if count, exists := archCount[arch]; exists && count > 0 {
			return arch, nil
		}
	}

	// If no priority arch found, return the most common architecture
	var mostCommonArch string
	maxCount := 0
	for arch, count := range archCount {
		if count > maxCount {
			maxCount = count
			mostCommonArch = arch
		}
	}

	return mostCommonArch, nil
}

// GetVaultImageForArchitecture returns the appropriate vault image for the given architecture
// Supports env-var override for disconnected clusters: VAULT_IMAGE=<registry>/vault:<tag>
func GetVaultImageForArchitecture(arch string) string {
	// Check for environment variable override first (for disconnected/internal registry)
	if envImage := os.Getenv("VAULT_IMAGE"); envImage != "" {
		return envImage
	}

	// Per-arch vault images. registry.connect.redhat.com/hashicorp/vault only has amd64/arm64
	// manifests; ppc64le and s390x require their respective IBM Container Registry images.
	vaultImages := map[string]string{
		"amd64":   "registry.connect.redhat.com/hashicorp/vault:1.17.3-ubi",
		"arm64":   "registry.connect.redhat.com/hashicorp/vault:1.17.3-ubi",
		"ppc64le": "icr.io/ppc64le-oss/vault-ppc64le:v1.14.8",
		"s390x":   "icr.io/s390x-oss/vault-s390x:v1.14.8",
	}
	if image, ok := vaultImages[arch]; ok {
		return image
	}
	return vaultImages["amd64"]
}

// ApplyManifestFromReaderWithImageSubstitution applies Kubernetes manifests from a reader function
// and substitutes container images based on the provided image map
// This supports embedded files via embed.FS
func ApplyManifestFromReaderWithImageSubstitution(ctx context.Context, dynamicClient dynamic.Interface, readFunc func(string) ([]byte, error), filePath string, imageSubstitutions map[string]string) error {
	return ApplyManifestFromReaderWithImageSubstitutionAndReplacements(ctx, dynamicClient, readFunc, filePath, imageSubstitutions, nil)
}

// ApplyManifestFromReaderWithImageSubstitutionAndReplacements applies Kubernetes manifests from a reader function
// with both image substitutions and string replacements for template variables
func ApplyManifestFromReaderWithImageSubstitutionAndReplacements(ctx context.Context, dynamicClient dynamic.Interface, readFunc func(string) ([]byte, error), filePath string, imageSubstitutions map[string]string, replacements map[string]string) error {
	// Read the file using the provided function
	data, err := readFunc(filePath)
	if err != nil {
		return fmt.Errorf("failed to read manifest file: %w", err)
	}

	// Apply string replacements first if provided
	if len(replacements) > 0 {
		content := string(data)
		for placeholder, value := range replacements {
			content = strings.ReplaceAll(content, placeholder, value)
		}
		data = []byte(content)
	}

	return ApplyManifestWithImageSubstitution(ctx, dynamicClient, data, imageSubstitutions)
}

// ApplyManifestWithImageSubstitution applies Kubernetes manifests from byte data
// and substitutes container images based on the provided image map
func ApplyManifestWithImageSubstitution(ctx context.Context, dynamicClient dynamic.Interface, data []byte, imageSubstitutions map[string]string) error {

	// Split YAML documents
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to decode manifest: %w", err)
		}

		// Skip empty documents
		if obj.Object == nil {
			continue
		}

		// Substitute images if this is a Deployment or StatefulSet
		if obj.GetKind() == "Deployment" || obj.GetKind() == "StatefulSet" {
			if err := substituteContainerImages(&obj, imageSubstitutions); err != nil {
				return fmt.Errorf("failed to substitute images: %w", err)
			}
		}

		// Get GVR (GroupVersionResource) from the object
		gvk := obj.GroupVersionKind()
		gvr := schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: pluralizeResource(gvk.Kind),
		}

		namespace := obj.GetNamespace()
		name := obj.GetName()

		// Try to get the resource first
		var resourceClient dynamic.ResourceInterface
		if namespace != "" {
			resourceClient = dynamicClient.Resource(gvr).Namespace(namespace)
		} else {
			resourceClient = dynamicClient.Resource(gvr)
		}

		existing, err := resourceClient.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			// Resource exists, update it
			obj.SetResourceVersion(existing.GetResourceVersion())
			_, err = resourceClient.Update(ctx, &obj, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update %s/%s: %w", gvk.Kind, name, err)
			}
		} else {
			// Resource doesn't exist, create it
			_, err = resourceClient.Create(ctx, &obj, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create %s/%s: %w", gvk.Kind, name, err)
			}
		}
	}

	return nil
}

// substituteContainerImages replaces container images in a Deployment or StatefulSet
// and adds node selector for the target architecture
func substituteContainerImages(obj *unstructured.Unstructured, imageSubstitutions map[string]string) error {
	// Get the containers from spec.template.spec.containers
	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("failed to get containers: %w", err)
	}
	if !found {
		return nil // No containers to substitute
	}

	// Iterate through containers and substitute images
	modified := false
	var targetArch string
	for i, container := range containers {
		containerMap, ok := container.(map[string]interface{})
		if !ok {
			continue
		}

		currentImage, found, err := unstructured.NestedString(containerMap, "image")
		if err != nil || !found {
			continue
		}

		// Check if we have a substitution for this image
		for oldImage, newImage := range imageSubstitutions {
			if strings.Contains(currentImage, oldImage) || currentImage == oldImage {
				containerMap["image"] = newImage
				containers[i] = containerMap
				modified = true

				// Determine target architecture from the new image
				if strings.Contains(newImage, "ppc64le") {
					targetArch = "ppc64le"
				} else if strings.Contains(newImage, "arm64") {
					targetArch = "arm64"
				} else if strings.Contains(newImage, "s390x") {
					targetArch = "s390x"
				}
				break
			}
		}
	}

	// Update the object if we made changes
	if modified {
		if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("failed to set containers: %w", err)
		}

		// Add node selector if we detected a specific architecture
		if targetArch != "" {
			nodeSelector := map[string]interface{}{
				"kubernetes.io/arch": targetArch,
			}
			if err := unstructured.SetNestedMap(obj.Object, nodeSelector, "spec", "template", "spec", "nodeSelector"); err != nil {
				return fmt.Errorf("failed to set nodeSelector: %w", err)
			}
		}
	}

	return nil
}
