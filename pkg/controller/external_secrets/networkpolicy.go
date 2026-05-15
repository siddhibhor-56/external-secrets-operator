package external_secrets

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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

	// TODO(siddhibhor-56,ESO-v1.4.0): Remove after 3 releases once the migration from
	// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
	if err := r.cleanupMigratedNetworkPolicies(esc, resourceMetadata); err != nil {
		return err
	}

	return nil
}

// createOrApplyStaticNetworkPolicies applies the static network policy manifests from bindata.
func (r *Reconciler) createOrApplyStaticNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	// Define static network policy assets to apply
	staticNetworkPolicies := []struct {
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
			assetName: allowDnsTrafficAssetName,
			condition: true,
		},
	}

	// Apply static network policies based on conditions
	for _, np := range staticNetworkPolicies {
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

	if exists && externalSecretsConfigCreateRecon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "NetworkPolicy %s already exists", networkPolicyName)
	}

	switch {
	case exists && common.HasObjectChanged(networkPolicy, fetched, &resourceMetadata):
		r.log.V(1).Info("NetworkPolicy modified, updating", "name", networkPolicyName)
		common.RemoveObsoleteAnnotations(networkPolicy, resourceMetadata)
		if err := r.UpdateWithRetry(r.ctx, networkPolicy); err != nil {
			return common.FromClientError(err, "failed to update network policy %s", networkPolicyName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s updated", networkPolicyName)
	case !exists:
		if err := r.Create(r.ctx, networkPolicy); err != nil {
			return common.FromClientError(err, "failed to create network policy %s", networkPolicyName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s created", networkPolicyName)
	default:
		r.log.V(4).Info("NetworkPolicy already up-to-date", "name", networkPolicyName)
	}

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

	if exists && externalSecretsConfigCreateRecon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "NetworkPolicy %s already exists", networkPolicyName)
	}

	switch {
	case exists && common.HasObjectChanged(networkPolicy, fetched, &resourceMetadata):
		r.log.V(1).Info("NetworkPolicy modified, updating", "name", networkPolicyName)
		common.RemoveObsoleteAnnotations(networkPolicy, resourceMetadata)
		if err := r.UpdateWithRetry(r.ctx, networkPolicy); err != nil {
			return common.FromClientError(err, "failed to update network policy %s", networkPolicyName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s updated", networkPolicyName)
	case !exists:
		if err := r.Create(r.ctx, networkPolicy); err != nil {
			return common.FromClientError(err, "failed to create network policy %s", networkPolicyName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s created", networkPolicyName)
	default:
		r.log.V(4).Info("NetworkPolicy already up-to-date", "name", networkPolicyName)
	}

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
func (r *Reconciler) reconcileProxyEgressPolicy(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, recon bool) error {
	namespace := getNamespace(esc)
	proxyConfig := r.getProxyConfiguration(esc)
	shouldExist := proxyConfig != nil && getNetworkPolicyProvisioning(proxyConfig) == operatorv1alpha1.ManagementStateManaged

	existing := &networkingv1.NetworkPolicy{}
	key := client.ObjectKey{Namespace: namespace, Name: proxyEgressPolicyName}
	exists, err := r.Exists(r.ctx, key, existing)
	if err != nil {
		return common.FromClientError(err, "failed to check existence of proxy egress network policy %s", proxyEgressPolicyName)
	}

	if !shouldExist {
		if exists {
			r.log.V(1).Info("removing proxy egress network policy", "name", proxyEgressPolicyName)
			if err := r.Delete(r.ctx, existing); err != nil {
				return common.FromClientError(err, "failed to delete proxy egress network policy %s", proxyEgressPolicyName)
			}
			r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "proxy egress NetworkPolicy %s removed", proxyEgressPolicyName)
		}
		return nil
	}

	np, err := buildProxyEgressNetworkPolicy(proxyConfig, namespace, resourceMetadata)
	if err != nil {
		return fmt.Errorf("failed to build proxy egress network policy: %w", err)
	}

	npName := fmt.Sprintf("%s/%s", namespace, proxyEgressPolicyName)
	switch {
	case exists && common.HasObjectChanged(np, existing, &resourceMetadata):
		r.log.V(1).Info("proxy egress NetworkPolicy modified, updating", "name", npName)
		common.RemoveObsoleteAnnotations(np, resourceMetadata)
		if err := r.UpdateWithRetry(r.ctx, np); err != nil {
			return common.FromClientError(err, "failed to update proxy egress network policy %s", npName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "proxy egress NetworkPolicy %s updated", npName)
	case !exists:
		if err := r.Create(r.ctx, np); err != nil {
			return common.FromClientError(err, "failed to create proxy egress network policy %s", npName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "proxy egress NetworkPolicy %s created", npName)
		if recon {
			r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "proxy egress NetworkPolicy %s already exists", npName)
		}
	default:
		r.log.V(4).Info("proxy egress NetworkPolicy already up-to-date", "name", npName)
	}

	return nil
}

// getNetworkPolicyProvisioning returns the effective provisioning state, defaulting to Managed.
func getNetworkPolicyProvisioning(proxyConfig *operatorv1alpha1.ProxyConfig) operatorv1alpha1.ManagementState {
	if proxyConfig.NetworkPolicyProvisioning == "" {
		return operatorv1alpha1.ManagementStateManaged
	}
	return proxyConfig.NetworkPolicyProvisioning
}

// buildProxyEgressNetworkPolicy constructs the eso-sys-proxy-egress-core NetworkPolicy
// that allows all ESO pods to reach the proxy server on its configured port.
func buildProxyEgressNetworkPolicy(proxyConfig *operatorv1alpha1.ProxyConfig, namespace string, resourceMetadata common.ResourceMetadata) (*networkingv1.NetworkPolicy, error) {
	port, err := extractProxyPort(proxyConfig)
	if err != nil {
		return nil, err
	}

	tcp := corev1.ProtocolTCP
	portVal := intstr.FromInt32(int32(port))

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
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &portVal},
					},
				},
			},
		},
	}
	common.ApplyResourceMetadata(np, resourceMetadata)
	return np, nil
}

// extractProxyPort returns the port from the first non-empty proxy URL in the config.
func extractProxyPort(proxyConfig *operatorv1alpha1.ProxyConfig) (int, error) {
	for _, raw := range []string{proxyConfig.HTTPSProxy, proxyConfig.HTTPProxy} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return 0, fmt.Errorf("failed to parse proxy URL %q: %w", raw, err)
		}
		if p := u.Port(); p != "" {
			var port int
			if _, err := fmt.Sscanf(p, "%d", &port); err == nil && port > 0 {
				return port, nil
			}
		}
		// Fall back to scheme-based default ports.
		switch strings.ToLower(u.Scheme) {
		case "https":
			return 443, nil
		case "http":
			return 80, nil
		}
	}
	return 3128, nil
}

// TODO(siddhibhor-56,ESO-v1.4.0): Remove after 3 releases once the migration from
// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
//
// desiredNetworkPolicyNames returns the set of Kubernetes NetworkPolicy object names that
// should exist for the current configuration. Used by cleanupMigratedNetworkPolicies to
// identify stale objects.
func (r *Reconciler) desiredNetworkPolicyNames(esc *operatorv1alpha1.ExternalSecretsConfig) map[string]struct{} {
	desired := map[string]struct{}{}
	staticAssets := []struct {
		assetName string
		condition bool
	}{
		{denyAllNetworkPolicyAssetName, true},
		{allowMainControllerTrafficAssetName, true},
		{allowWebhookTrafficAssetName, true},
		{allowCertControllerTrafficAssetName, !isCertManagerConfigEnabled(esc)},
		{allowBitwardenServerTrafficAssetName, isBitwardenConfigEnabled(esc)},
		{allowDnsTrafficAssetName, true},
	}
	for _, s := range staticAssets {
		if s.condition {
			np := common.DecodeNetworkPolicyObjBytes(assets.MustAsset(s.assetName))
			desired[np.GetName()] = struct{}{}
		}
	}

	proxyConfig := r.getProxyConfiguration(esc)
	if proxyConfig != nil && getNetworkPolicyProvisioning(proxyConfig) == operatorv1alpha1.ManagementStateManaged {
		desired[proxyEgressPolicyName] = struct{}{}
	}

	for _, npConfig := range esc.Spec.ControllerConfig.NetworkPolicies {
		desired[userNetworkPolicyPrefix+npConfig.Name] = struct{}{}
	}
	return desired
}

// TODO(siddhibhor-56,ESO-v1.4.0): Remove after 3 releases once the migration from
// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
//
// cleanupMigratedNetworkPolicies removes NetworkPolicy objects that are owned by the operator
// (identified by the managed-by label) but are not in the current desired set. This handles
// the migration from unprefixed names (pre-1.2.0) to the eso-sys-/eso-user- naming scheme
// and also prunes stale user NPs that have been removed from the CR spec.
//
// The cleanup runs only once per CR lifetime: after a successful pass the
// skipNPCleanupAnnotation is written to the CR so subsequent reconciles skip the loop.
func (r *Reconciler) cleanupMigratedNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata) error {
	if esc.GetAnnotations()[skipNPCleanupAnnotation] == "true" {
		return nil
	}

	namespace := getNamespace(esc)
	desired := r.desiredNetworkPolicyNames(esc)

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
		if _, ok := desired[np.GetName()]; ok {
			continue
		}
		r.log.V(1).Info("deleting stale/unprefixed network policy", "name", np.GetName(), "namespace", namespace)
		if err := r.Delete(r.ctx, np); err != nil {
			return common.FromClientError(err, "failed to delete stale network policy %s/%s", namespace, np.GetName())
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "stale NetworkPolicy %s/%s removed", namespace, np.GetName())
	}

	return r.markCleanupDone(esc)
}

// TODO(siddhibhor-56,ESO-v1.4.0): Remove after 3 releases once the migration from
// unprefixed to eso-sys-/eso-user- network policy names is no longer needed.
//
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
