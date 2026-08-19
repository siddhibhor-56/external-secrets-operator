package external_secrets

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	"github.com/openshift/external-secrets-operator/pkg/operator/assets"
)

// createOrApplyNetworkPolicies handles creation of both static network policies from manifests,
// the conditional proxy egress policy, and custom network policies configured in the
// ExternalSecretsConfig API. After all desired policies are applied it runs a one-time
// migration cleanup to remove unprefixed policies left by operator versions prior to 1.2.0.
func (r *Reconciler) createOrApplyNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	if err := r.createOrApplyStaticNetworkPolicies(esc, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
		return err
	}

	if err := r.reconcileProxyEgressPolicy(esc, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
		return err
	}

	if err := r.createOrApplyCustomNetworkPolicies(esc, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
		return err
	}

	// TODO: Remove after 3 releases(in v1.5.0) once the migration from
	// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
	if err := r.cleanupMigratedNetworkPolicies(esc); err != nil {
		return err
	}

	return nil
}

// staticNetworkPolicyAssetConfigs returns static network policy bindata assets and whether each
// should be applied for the current ExternalSecretsConfig.
func staticNetworkPolicyAssetConfigs(esc *operatorv1alpha1.ExternalSecretsConfig) []struct {
	assetName string
	condition bool
} {
	return []struct {
		assetName string
		condition bool
	}{
		{
			assetName: denyAllNetworkPolicyAssetName,
			condition: true, // Always apply deny-all as the base policy
		},
		{
			assetName: allowMainControllerTrafficAssetName,
			condition: true, // Always apply for main controller
		},
		{
			assetName: allowWebhookTrafficAssetName,
			condition: true, // Always apply for webhook
		},
		{
			assetName: allowCertControllerTrafficAssetName,
			condition: !isCertManagerConfigEnabled(esc), // Only if cert-controller is enabled
		},
		{
			assetName: allowBitwardenServerTrafficAssetName,
			condition: isBitwardenConfigEnabled(esc), // Only if bitwarden is enabled
		},
		{
			assetName: allowDNSTrafficAssetName,
			condition: true,
		},
	}
}

// createOrApplyStaticNetworkPolicies applies the static network policy manifests from bindata.
func (r *Reconciler) createOrApplyStaticNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	for _, np := range staticNetworkPolicyAssetConfigs(esc) {
		if !np.condition {
			continue
		}
		if err := r.createOrApplyNetworkPolicyFromAsset(esc, np.assetName, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
			return err
		}
	}

	return nil
}

// createOrApplyCustomNetworkPolicies applies custom network policies defined in the ExternalSecretsConfig spec.
func (r *Reconciler) createOrApplyCustomNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	if esc.Spec.ControllerConfig.NetworkPolicies == nil {
		r.log.V(4).Info("No custom network policies configured in ControllerConfig")
		return nil
	}

	for _, npConfig := range esc.Spec.ControllerConfig.NetworkPolicies {
		if err := r.createOrApplyCustomNetworkPolicy(esc, npConfig, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
			return err
		}
	}

	return nil
}

// createOrApplyCustomNetworkPolicy creates or updates a custom network policy based on API configuration.
func (r *Reconciler) createOrApplyCustomNetworkPolicy(esc *operatorv1alpha1.ExternalSecretsConfig, npConfig operatorv1alpha1.NetworkPolicy, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	// Build the NetworkPolicy object from the API spec
	networkPolicy, err := r.buildNetworkPolicyFromConfig(esc, npConfig, resourceMetadata)
	if err != nil {
		return err
	}

	networkPolicyName := fmt.Sprintf("%s/%s", networkPolicy.GetNamespace(), networkPolicy.GetName())
	r.log.V(4).Info("Reconciling custom network policy", "name", networkPolicyName, "component", npConfig.ComponentName)

	fetched := &networkingv1.NetworkPolicy{}
	exists, err := r.Exists(r.ctx, client.ObjectKeyFromObject(networkPolicy), fetched)
	if err != nil {
		return common.FromClientError(err, "failed to check existence of network policy %s", networkPolicyName)
	}

	if !exists {
		return r.createWithFallback(networkPolicy, resourceMetadata, networkPolicyName, esc)
	}

	if externalSecretsConfigCreateRecon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "NetworkPolicy %s already exists", networkPolicyName)
	}

	if !common.HasObjectChanged(networkPolicy, fetched, &resourceMetadata) {
		r.log.V(4).Info("NetworkPolicy already up-to-date", "name", networkPolicyName)
		return nil
	}

	r.log.V(1).Info("NetworkPolicy modified, updating", "name", networkPolicyName)
	common.RemoveObsoleteAnnotations(networkPolicy, resourceMetadata)
	if err := r.UpdateWithRetry(r.ctx, networkPolicy); err != nil {
		return common.FromClientError(err, "failed to update network policy %s", networkPolicyName)
	}
	r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s updated", networkPolicyName)

	return nil
}

// createOrApplyNetworkPolicyFromAsset decodes a NetworkPolicy YAML asset and ensures it exists in the cluster.
func (r *Reconciler) createOrApplyNetworkPolicyFromAsset(esc *operatorv1alpha1.ExternalSecretsConfig, assetName string, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	networkPolicy := common.DecodeNetworkPolicyObjBytes(assets.MustAsset(assetName))
	updateNamespace(networkPolicy, esc)
	common.ApplyResourceMetadata(networkPolicy, resourceMetadata)

	networkPolicyName := fmt.Sprintf("%s/%s", networkPolicy.GetNamespace(), networkPolicy.GetName())
	r.log.V(4).Info("Reconciling static network policy", "name", networkPolicyName)

	fetched := &networkingv1.NetworkPolicy{}
	exists, err := r.Exists(r.ctx, client.ObjectKeyFromObject(networkPolicy), fetched)
	if err != nil {
		return common.FromClientError(err, "failed to check existence of network policy %s", networkPolicyName)
	}

	if !exists {
		return r.createWithFallback(networkPolicy, resourceMetadata, networkPolicyName, esc)
	}

	if externalSecretsConfigCreateRecon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "NetworkPolicy %s already exists", networkPolicyName)
	}

	if !common.HasObjectChanged(networkPolicy, fetched, &resourceMetadata) {
		r.log.V(4).Info("NetworkPolicy already up-to-date", "name", networkPolicyName)
		return nil
	}

	r.log.V(1).Info("NetworkPolicy modified, updating", "name", networkPolicyName)
	common.RemoveObsoleteAnnotations(networkPolicy, resourceMetadata)
	if err := r.UpdateWithRetry(r.ctx, networkPolicy); err != nil {
		return common.FromClientError(err, "failed to update network policy %s", networkPolicyName)
	}
	r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s updated", networkPolicyName)

	return nil
}

// buildNetworkPolicyFromConfig constructs a NetworkPolicy object from the API configuration.
func (r *Reconciler) buildNetworkPolicyFromConfig(esc *operatorv1alpha1.ExternalSecretsConfig, npConfig operatorv1alpha1.NetworkPolicy, resourceMetadata common.ResourceMetadata) (*networkingv1.NetworkPolicy, error) {
	namespace := getNamespace(esc)

	// Determine pod selector based on component name
	podSelector, err := r.getPodSelectorForComponent(npConfig.ComponentName)
	if err != nil {
		return nil, fmt.Errorf("failed to determine pod selector for network policy %s: %w", npConfig.Name, err)
	}

	// Build the NetworkPolicy object
	networkPolicy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      userNetworkPolicyPrefix + npConfig.Name,
			Namespace: namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podSelector,
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: npConfig.Egress,
		},
	}
	common.ApplyResourceMetadata(networkPolicy, resourceMetadata)
	return networkPolicy, nil
}

// getPodSelectorForComponent returns the appropriate pod selector for the given component.
func (r *Reconciler) getPodSelectorForComponent(componentName operatorv1alpha1.ComponentName) (metav1.LabelSelector, error) {
	switch componentName {
	case operatorv1alpha1.CoreController:
		return metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name": "external-secrets",
			},
		}, nil
	case operatorv1alpha1.BitwardenSDKServer:
		return metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name": "bitwarden-sdk-server",
			},
		}, nil
	default:
		return metav1.LabelSelector{}, fmt.Errorf("unknown component name: %s", componentName)
	}
}

// reconcileProxyEgressPolicy creates, updates, or removes the automatic proxy egress
// NetworkPolicy depending on whether a proxy is configured and the management state.
// When only NO_PROXY is set (no HTTP/HTTPS proxy URLs), no egress policy is created
// because there is no proxy endpoint to allow traffic to.
func (r *Reconciler) reconcileProxyEgressPolicy(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, recon bool) error {
	namespace := getNamespace(esc)
	shouldExist := r.proxyConfig != nil &&
		hasProxyEndpointURLs(r.proxyConfig) &&
		getNetworkPolicyProvisioning(r.proxyConfig) == operatorv1alpha1.ManagementStateManaged
	npName := fmt.Sprintf("%s/%s", namespace, proxyEgressPolicyName)

	existing := &networkingv1.NetworkPolicy{}
	key := client.ObjectKey{Namespace: namespace, Name: proxyEgressPolicyName}
	exists, err := r.Exists(r.ctx, key, existing)
	if err != nil {
		return common.FromClientError(err, "failed to check existence of proxy egress network policy %s", npName)
	}

	if !shouldExist {
		if exists {
			r.log.V(1).Info("removing proxy egress network policy", "name", npName)
			if err := r.Delete(r.ctx, existing); err != nil {
				return common.FromClientError(err, "failed to delete proxy egress network policy %s", npName)
			}
			r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "proxy egress NetworkPolicy %s removed", npName)
		}
		return nil
	}

	np := buildProxyEgressNetworkPolicy(r.proxyConfig, namespace, resourceMetadata)

	if !exists {
		return r.createWithFallback(np, resourceMetadata, npName, esc)
	}

	if recon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "proxy egress NetworkPolicy %s already exists", npName)
	}

	if !common.HasObjectChanged(np, existing, &resourceMetadata) {
		r.log.V(4).Info("proxy egress NetworkPolicy already up-to-date", "name", npName)
		return nil
	}

	r.log.V(1).Info("proxy egress NetworkPolicy modified, updating", "name", npName)
	common.RemoveObsoleteAnnotations(np, resourceMetadata)
	if err := r.UpdateWithRetry(r.ctx, np); err != nil {
		return common.FromClientError(err, "failed to update proxy egress network policy %s", npName)
	}
	r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "proxy egress NetworkPolicy %s updated", npName)

	return nil
}

// getNetworkPolicyProvisioning returns the effective provisioning state, defaulting to Managed.
func getNetworkPolicyProvisioning(proxyConfig *operatorv1alpha1.ProxyConfig) operatorv1alpha1.ManagementState {
	if proxyConfig.NetworkPolicyProvisioning == "" {
		return operatorv1alpha1.ManagementStateManaged
	}
	return proxyConfig.NetworkPolicyProvisioning
}

// buildProxyEgressNetworkPolicy constructs the eso-sys-allow-proxy-egress NetworkPolicy
// that allows all ESO pods to reach the proxy server(s) on their configured port(s).
// When HTTPSProxy and HTTPProxy use different ports, both are included as egress rules.
// proxyConfig must already be validated by resolveProxyConfiguration.
func buildProxyEgressNetworkPolicy(proxyConfig *operatorv1alpha1.ProxyConfig, namespace string, resourceMetadata common.ResourceMetadata) *networkingv1.NetworkPolicy {
	ports := extractProxyPorts(proxyConfig)
	if len(ports) == 0 {
		return nil
	}

	egressPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, port := range ports {
		egressPorts = append(egressPorts, networkingv1.NetworkPolicyPort{
			Protocol: ptr.To(corev1.ProtocolTCP),
			Port:     ptr.To(intstr.FromInt32(int32(port))),
		})
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyEgressPolicyName,
			Namespace: namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "app.kubernetes.io/name",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"external-secrets", "external-secrets-webhook", "external-secrets-cert-controller", "bitwarden-sdk-server"},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: egressPorts,
				},
			},
		},
	}
	common.ApplyResourceMetadata(np, resourceMetadata)
	return np
}

// extractProxyPorts returns the set of TCP ports for the proxy egress NetworkPolicy,
// derived from both HTTPSProxy and HTTPProxy URLs in proxyConfig. An explicit port in a
// URL is used directly; otherwise scheme defaults apply (443 for https, 80 for http).
// Returns an empty slice when neither proxy URL yields a port (e.g. only NO_PROXY is
// configured). proxyConfig must already be validated by resolveProxyConfiguration.
func extractProxyPorts(proxyConfig *operatorv1alpha1.ProxyConfig) []int {
	seen := map[int]struct{}{}
	var ports []int

	for _, raw := range []string{proxyConfig.HTTPSProxy, proxyConfig.HTTPProxy} {
		if raw == "" {
			continue
		}
		u, _ := url.Parse(raw)
		port := 0
		if p := u.Port(); p != "" {
			port, _ = strconv.Atoi(p)
		}
		if port == 0 {
			switch strings.ToLower(u.Scheme) {
			case "https":
				port = 443
			case "http":
				port = 80
			}
		}
		if port > 0 {
			if _, exists := seen[port]; !exists {
				seen[port] = struct{}{}
				ports = append(ports, port)
			}
		}
	}

	return ports
}

// TODO: Remove after 3 releases(in v1.5.0) once the migration from
// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
//
// legacyOperatorNetworkPolicyNames returns unprefixed NetworkPolicy object names that may
// exist from operator versions prior to the eso-sys-/eso-user- naming scheme. Used as a
// delete allowlist by cleanupMigratedNetworkPolicies.
func legacyOperatorNetworkPolicyNames(esc *operatorv1alpha1.ExternalSecretsConfig) map[string]struct{} {
	legacy := map[string]struct{}{}
	for _, s := range staticNetworkPolicyAssetConfigs(esc) {
		if !s.condition {
			continue
		}
		np := common.DecodeNetworkPolicyObjBytes(assets.MustAsset(s.assetName))
		legacy[strings.TrimPrefix(np.GetName(), systemNetworkPolicyPrefix)] = struct{}{}
	}
	for _, npConfig := range esc.Spec.ControllerConfig.NetworkPolicies {
		legacy[npConfig.Name] = struct{}{}
	}
	return legacy
}

// TODO: Remove after 3 releases(in v1.5.0) once the migration from
// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
//
// cleanupMigratedNetworkPolicies removes legacy unprefixed NetworkPolicy objects created by
// prior operator versions. Deletion is limited to an explicit allowlist of legacy names
// (static bindata names with eso-sys- stripped, and CR networkPolicies names as-is) and
// operator ownership labels so user-managed policies are not affected.
//
// The cleanup runs only once per CR lifetime: after a successful pass the
// skipNPCleanupAnnotation is written to the CR so subsequent reconciles skip the loop.
func (r *Reconciler) cleanupMigratedNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig) error {
	if esc.GetAnnotations()[skipNPCleanupAnnotation] == "true" {
		return nil
	}

	namespace := getNamespace(esc)
	legacy := legacyOperatorNetworkPolicyNames(esc)

	var npList networkingv1.NetworkPolicyList
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels{
			"app.kubernetes.io/managed-by": common.ExternalSecretsOperatorCommonName,
			"app.kubernetes.io/part-of":    common.ExternalSecretsOperatorCommonName,
		},
	}
	if err := r.List(r.ctx, &npList, listOpts...); err != nil {
		return common.FromClientError(err, "failed to list network policies in %s for cleanup", namespace)
	}

	for i := range npList.Items {
		np := &npList.Items[i]
		if _, ok := legacy[np.GetName()]; !ok {
			continue
		}
		r.log.V(1).Info("deleting legacy unprefixed network policy", "name", np.GetName(), "namespace", namespace)
		if err := r.Delete(r.ctx, np); err != nil {
			if apierrors.IsNotFound(err) {
				r.log.V(4).Info("legacy network policy already deleted", "name", np.GetName(), "namespace", namespace)
				continue
			}
			return common.FromClientError(err, "failed to delete legacy network policy %s/%s", namespace, np.GetName())
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "legacy NetworkPolicy %s/%s removed", namespace, np.GetName())
	}

	return r.markCleanupDone(esc)
}

// markCleanupDone patches the skipNPCleanupAnnotation onto the CR so the cleanup loop
// is skipped on future reconciles.
func (r *Reconciler) markCleanupDone(esc *operatorv1alpha1.ExternalSecretsConfig) error {
	patchBody := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				skipNPCleanupAnnotation: "true",
			},
		},
	}
	patchBytes, err := json.Marshal(patchBody)
	if err != nil {
		return fmt.Errorf("failed to marshal cleanup annotation patch: %w", err)
	}
	patch := client.RawPatch(types.MergePatchType, patchBytes)
	if err := r.Patch(r.ctx, esc, patch, client.FieldOwner(common.ExternalSecretsOperatorCommonName)); err != nil {
		return fmt.Errorf("failed to patch %s annotation on CR: %w", skipNPCleanupAnnotation, err)
	}
	r.log.V(1).Info("marked network policy cleanup as done", "annotation", skipNPCleanupAnnotation)
	return nil
}
