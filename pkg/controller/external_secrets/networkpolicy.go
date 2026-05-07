package external_secrets

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

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

// createOrApplyNetworkPolicies handles creation of both static network policies from manifests
// and custom network policies configured in the ExternalSecretsConfig API.
func (r *Reconciler) createOrApplyNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	if err := r.createOrApplyStaticNetworkPolicies(esc, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
		return err
	}

	if err := r.createOrApplyCustomNetworkPolicies(esc, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
		return err
	}

	if err := r.createOrApplyProxyEgressNetworkPolicy(esc, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
		return err
	}

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
			assetName: allowDnsTrafficAsserName,
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
// The resulting K8s object name is prefixed with userNetworkPolicyPrefix (eso-user-) so that
// operator-owned user policies are unambiguously identified and prunable by cleanupMigratedNetworkPolicies.
func (r *Reconciler) buildNetworkPolicyFromConfig(esc *operatorv1alpha1.ExternalSecretsConfig, npConfig operatorv1alpha1.NetworkPolicy, resourceMetadata common.ResourceMetadata) (*networkingv1.NetworkPolicy, error) {
	namespace := getNamespace(esc)

	podSelector, err := r.getPodSelectorForComponent(npConfig.ComponentName)
	if err != nil {
		return nil, fmt.Errorf("failed to determine pod selector for network policy %s: %w", npConfig.Name, err)
	}

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

// createOrApplyProxyEgressNetworkPolicy manages the eso-sys-proxy-egress-core NetworkPolicy.
// It is created when spec.appConfig.proxy.networkPolicyAllowProxyEgressAll is Managed (default)
// AND an effective proxy is configured. When either condition is not met, any existing policy
// is deleted to avoid stale allow rules.
func (r *Reconciler) createOrApplyProxyEgressNetworkPolicy(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	proxyConfig := r.getProxyConfiguration(esc)
	namespace := getNamespace(esc)
	npName := fmt.Sprintf("%s/%s", namespace, proxyEgressNetworkPolicyName)

	if !shouldManageProxyEgress(esc, proxyConfig) {
		fetched := &networkingv1.NetworkPolicy{}
		exists, err := r.Exists(r.ctx, types.NamespacedName{Name: proxyEgressNetworkPolicyName, Namespace: namespace}, fetched)
		if err != nil {
			return common.FromClientError(err, "failed to check existence of proxy egress network policy %s", npName)
		}
		if exists {
			r.log.V(1).Info("Proxy egress policy no longer needed, deleting", "name", npName)
			if err := r.Delete(r.ctx, fetched); err != nil {
				return common.FromClientError(err, "failed to delete proxy egress network policy %s", npName)
			}
			r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s deleted", npName)
		}
		return nil
	}

	networkPolicy := r.buildProxyEgressNetworkPolicy(esc, proxyConfig, resourceMetadata)
	r.log.V(4).Info("Reconciling proxy egress network policy", "name", npName)

	fetched := &networkingv1.NetworkPolicy{}
	exists, err := r.Exists(r.ctx, client.ObjectKeyFromObject(networkPolicy), fetched)
	if err != nil {
		return common.FromClientError(err, "failed to check existence of proxy egress network policy %s", npName)
	}

	if exists && externalSecretsConfigCreateRecon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "NetworkPolicy %s already exists", npName)
	}

	switch {
	case exists && common.HasObjectChanged(networkPolicy, fetched, &resourceMetadata):
		r.log.V(1).Info("Proxy egress NetworkPolicy modified, updating", "name", npName)
		common.RemoveObsoleteAnnotations(networkPolicy, resourceMetadata)
		if err := r.UpdateWithRetry(r.ctx, networkPolicy); err != nil {
			return common.FromClientError(err, "failed to update proxy egress network policy %s", npName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s updated", npName)
	case !exists:
		if err := r.Create(r.ctx, networkPolicy); err != nil {
			return common.FromClientError(err, "failed to create proxy egress network policy %s", npName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "NetworkPolicy %s created", npName)
	default:
		r.log.V(4).Info("Proxy egress NetworkPolicy already up-to-date", "name", npName)
	}

	return nil
}

// buildProxyEgressNetworkPolicy constructs the eso-sys-proxy-egress-core NetworkPolicy.
// The proxy port is extracted at reconcile time from the effective proxy configuration.
func (r *Reconciler) buildProxyEgressNetworkPolicy(esc *operatorv1alpha1.ExternalSecretsConfig, proxyConfig *operatorv1alpha1.ProxyConfig, resourceMetadata common.ResourceMetadata) *networkingv1.NetworkPolicy {
	port := intstr.FromInt32(getProxyPort(proxyConfig))
	protocol := corev1.ProtocolTCP

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      proxyEgressNetworkPolicyName,
			Namespace: getNamespace(esc),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": externalsecretsCommonName,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &protocol, Port: &port},
					},
				},
			},
		},
	}
	common.ApplyResourceMetadata(np, resourceMetadata)
	return np
}

// shouldManageProxyEgress returns true when the operator should create the proxy egress NetworkPolicy.
// Both conditions must hold: an effective proxy is configured AND the management mode is Managed (default).
func shouldManageProxyEgress(esc *operatorv1alpha1.ExternalSecretsConfig, proxyConfig *operatorv1alpha1.ProxyConfig) bool {
	if proxyConfig == nil {
		return false
	}
	if esc.Spec.ApplicationConfig.Proxy == nil {
		// Proxy came from ESM or OLM env vars; default management mode is Managed.
		return true
	}
	mode := esc.Spec.ApplicationConfig.Proxy.NetworkPolicyAllowProxyEgressAll
	return mode == "" || mode == operatorv1alpha1.Managed
}

// getProxyPort extracts the TCP port from the proxy URL.
// It tries HTTPSProxy first, then HTTPProxy. Falls back to 3128 (common squid default).
func getProxyPort(proxyConfig *operatorv1alpha1.ProxyConfig) int32 {
	for _, rawURL := range []string{proxyConfig.HTTPSProxy, proxyConfig.HTTPProxy} {
		if rawURL == "" {
			continue
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		portStr := u.Port()
		if portStr != "" {
			port, err := strconv.ParseInt(portStr, 10, 32)
			if err == nil && port > 0 {
				return int32(port)
			}
		}
		switch u.Scheme {
		case "https":
			return 443
		case "http":
			return 80
		}
	}
	return 3128
}

// cleanupMigratedNetworkPolicies prunes stale operator-managed NetworkPolicies from the operand namespace.
// On every reconcile it lists all NPs with the operator's managed-by label and deletes any whose name
// is not in the current desired set (catches entries removed from the CR spec).
// On the first reconcile after upgrade (no migrationCompleteAnnotation on the CR), it also deletes any
// legacy NetworkPolicies listed without the label. Once cleanup succeeds, the annotation is set.
func (r *Reconciler) cleanupMigratedNetworkPolicies(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata) error {
	namespace := getNamespace(esc)
	desired := r.desiredNetworkPolicyNames(esc, r.getProxyConfiguration(esc))

	// Always: label-based list to catch stale entries.
	labeledList := &networkingv1.NetworkPolicyList{}
	if err := r.List(r.ctx, labeledList,
		client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/managed-by": common.ExternalSecretsOperatorCommonName},
	); err != nil {
		return common.FromClientError(err, "failed to list network policies in namespace %s", namespace)
	}

	for i := range labeledList.Items {
		np := &labeledList.Items[i]
		if _, ok := desired[np.Name]; !ok {
			r.log.V(1).Info("Pruning stale network policy", "name", np.Name, "namespace", namespace)
			if err := r.Delete(r.ctx, np); err != nil {
				return common.FromClientError(err, "failed to delete stale network policy %s/%s", namespace, np.Name)
			}
		}
	}

	// First-reconcile migration: clean up legacy unprefixed names that lacked the managed-by label.
	if _, hasMigration := esc.GetAnnotations()[migrationCompleteAnnotation]; !hasMigration {
		if err := r.deleteLegacyNetworkPolicies(namespace, desired); err != nil {
			return err
		}
		if err := r.setMigrationCompleteAnnotation(esc); err != nil {
			return err
		}
	}

	return nil
}

// desiredNetworkPolicyNames returns the set of NetworkPolicy names currently managed by the operator.
func (r *Reconciler) desiredNetworkPolicyNames(esc *operatorv1alpha1.ExternalSecretsConfig, proxyConfig *operatorv1alpha1.ProxyConfig) map[string]struct{} {
	names := make(map[string]struct{})

	// Static bindata-based NPs.
	for name := range staticNetworkPolicyNames(esc) {
		names[name] = struct{}{}
	}

	// Proxy egress NP.
	if shouldManageProxyEgress(esc, proxyConfig) {
		names[proxyEgressNetworkPolicyName] = struct{}{}
	}

	// User-defined NPs (with eso-user- prefix).
	for _, np := range esc.Spec.ControllerConfig.NetworkPolicies {
		names[userNetworkPolicyPrefix+np.Name] = struct{}{}
	}

	return names
}

// staticNetworkPolicyNames returns the set of NP names created from bindata assets for this ESC.
func staticNetworkPolicyNames(esc *operatorv1alpha1.ExternalSecretsConfig) map[string]struct{} {
	names := map[string]struct{}{
		"deny-all-traffic":                            {},
		"allow-api-server-egress-for-main-controller": {},
		"allow-api-server-egress-for-webhook":         {},
		"allow-to-dns":                                {},
	}
	if !isCertManagerConfigEnabled(esc) {
		names["allow-api-server-egress-for-cert-controller"] = struct{}{}
	}
	if isBitwardenConfigEnabled(esc) {
		names["allow-api-server-egress-for-bitwarden-server"] = struct{}{}
	}
	return names
}

// deleteLegacyNetworkPolicies removes legacy NetworkPolicies by name that predate the eso-user- prefix scheme.
// These NPs were created without the managed-by label so they won't appear in the label-based list.
func (r *Reconciler) deleteLegacyNetworkPolicies(namespace string, desired map[string]struct{}) error {
	legacyNames := []string{
		"deny-all-traffic",
		"allow-api-server-egress",
		"allow-api-server-egress-for-webhook",
		"allow-api-server-egress-for-cert-controller",
		"allow-api-server-egress-for-bitwarden-sever",
		"allow-to-dns",
	}
	for _, name := range legacyNames {
		if _, isDesired := desired[name]; isDesired {
			continue
		}
		np := &networkingv1.NetworkPolicy{}
		exists, err := r.Exists(r.ctx, types.NamespacedName{Name: name, Namespace: namespace}, np)
		if err != nil {
			return common.FromClientError(err, "failed to check legacy network policy %s/%s", namespace, name)
		}
		if exists {
			r.log.V(1).Info("Deleting legacy network policy", "name", name, "namespace", namespace)
			if err := r.Delete(r.ctx, np); err != nil {
				return common.FromClientError(err, "failed to delete legacy network policy %s/%s", namespace, name)
			}
		}
	}
	return nil
}

// setMigrationCompleteAnnotation patches the migrationCompleteAnnotation onto the ExternalSecretsConfig CR.
func (r *Reconciler) setMigrationCompleteAnnotation(esc *operatorv1alpha1.ExternalSecretsConfig) error {
	patchBody := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				migrationCompleteAnnotation: "true",
			},
		},
	}
	patchBytes, err := json.Marshal(patchBody)
	if err != nil {
		return fmt.Errorf("failed to marshal migration annotation patch: %w", err)
	}
	patch := client.RawPatch(types.MergePatchType, patchBytes)
	if err := r.Patch(r.ctx, esc, patch, client.FieldOwner(common.ExternalSecretsOperatorCommonName)); err != nil {
		return common.FromClientError(err, "failed to set migration complete annotation on %s", esc.GetName())
	}
	r.log.V(4).Info("Network policy migration annotation set", "name", esc.GetName())
	return nil
}
