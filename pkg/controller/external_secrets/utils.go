package external_secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"go.uber.org/zap/zapcore"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
)

func getNamespace(_ *operatorv1alpha1.ExternalSecretsConfig) string {
	return OperandDefaultNamespace
}

func updateNamespace(obj client.Object, esc *operatorv1alpha1.ExternalSecretsConfig) {
	obj.SetNamespace(getNamespace(esc))
}

func containsProcessedAnnotation(esc *operatorv1alpha1.ExternalSecretsConfig) bool {
	_, exist := esc.GetAnnotations()[controllerProcessedAnnotation]
	return exist
}

func addProcessedAnnotation(esc *operatorv1alpha1.ExternalSecretsConfig) bool {
	annotations := esc.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 1)
	}
	if _, exist := annotations[controllerProcessedAnnotation]; !exist {
		annotations[controllerProcessedAnnotation] = "true"
		esc.SetAnnotations(annotations)
		return true
	}
	return false
}

func (r *Reconciler) updateCondition(esc *operatorv1alpha1.ExternalSecretsConfig, prependErr error) error {
	if err := r.updateStatus(r.ctx, esc); err != nil {
		errUpdate := fmt.Errorf("failed to update %s/%s status: %w", esc.GetNamespace(), esc.GetName(), err)
		if prependErr != nil {
			return utilerrors.NewAggregate([]error{prependErr, errUpdate})
		}
		return errUpdate
	}
	return nil
}

// updateStatus is for updating the status subresource of externalsecretsconfigs.operator.openshift.io.
func (r *Reconciler) updateStatus(ctx context.Context, changed *operatorv1alpha1.ExternalSecretsConfig) error {
	namespacedName := client.ObjectKeyFromObject(changed)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		r.log.V(4).Info("updating externalsecretsconfigs.operator.openshift.io status", "request", namespacedName)
		current := &operatorv1alpha1.ExternalSecretsConfig{}
		if err := r.Get(ctx, namespacedName, current); err != nil {
			return fmt.Errorf("failed to fetch externalsecretsconfigs.operator.openshift.io %q for status update: %w", namespacedName, err)
		}
		changed.Status.DeepCopyInto(&current.Status)

		if err := r.StatusUpdate(ctx, current); err != nil {
			return fmt.Errorf("failed to update externalsecretsconfigs.operator.openshift.io %q status: %w", namespacedName, err)
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// validateExternalSecretsConfig validates ExternalSecretsConfig CR fields (apart from CEL
// validations in the CRD).
func (r *Reconciler) validateExternalSecretsConfig(esc *operatorv1alpha1.ExternalSecretsConfig) error {
	gvk := esc.GetObjectKind().GroupVersionKind().String()
	name := esc.GetName()

	if isCertManagerConfigEnabled(esc) {
		if _, ok := r.optionalResourcesList[certificateCRDGKV]; !ok {
			return common.NewIrrecoverableError(
				fmt.Errorf("spec.controllerConfig.certProvider.certManager.mode is set, but cert-manager is not installed"),
				"%s/%s cert-manager configuration validation failed", gvk, name)
		}
	}

	proxyConfig, err := r.resolveProxyConfiguration(esc)
	if err != nil {
		return common.NewUserConfigurationError(err, "%s/%s proxy configuration validation failed", gvk, name)
	}
	r.proxyConfig = proxyConfig

	return nil
}

// isCertManagerConfigEnabled returns whether CertManagerConfig is enabled in ExternalSecretsConfig CR Spec.
func isCertManagerConfigEnabled(esc *operatorv1alpha1.ExternalSecretsConfig) bool {
	return esc.Spec.ControllerConfig.CertProvider != nil &&
		esc.Spec.ControllerConfig.CertProvider.CertManager != nil &&
		common.EvalMode(esc.Spec.ControllerConfig.CertProvider.CertManager.Mode)
}

// isBitwardenConfigEnabled returns whether BitwardenSecretManagerProvider is enabled in ExternalSecretsConfig CR Spec.
func isBitwardenConfigEnabled(esc *operatorv1alpha1.ExternalSecretsConfig) bool {
	return esc.Spec.Plugins.BitwardenSecretManagerProvider != nil &&
		common.EvalMode(esc.Spec.Plugins.BitwardenSecretManagerProvider.Mode)
}

func getLogLevel(esc *operatorv1alpha1.ExternalSecretsConfig, esm *operatorv1alpha1.ExternalSecretsManager) string {
	var logLevel int32 = 1
	if esc.Spec.ApplicationConfig.LogLevel != 0 {
		logLevel = esc.Spec.ApplicationConfig.LogLevel
	} else if esm != nil && esm.Spec.GlobalConfig != nil && esm.Spec.GlobalConfig.LogLevel != 0 {
		logLevel = esm.Spec.GlobalConfig.LogLevel
	}
	switch logLevel {
	case 0, 1, 2:
		return zapcore.Level(logLevel).String()
	case 4, 5:
		return zapcore.DebugLevel.String()
	}
	return zapcore.InfoLevel.String()
}

func getOperatingNamespace(esc *operatorv1alpha1.ExternalSecretsConfig) string {
	return esc.Spec.ApplicationConfig.OperatingNamespace
}

func (r *Reconciler) IsCertManagerInstalled() bool {
	_, ok := r.optionalResourcesList[certificateCRDGKV]
	return ok
}

// resolveProxyConfiguration returns the proxy configuration by layering sources in precedence
// order: ExternalSecretsConfig, then ExternalSecretsManager, then OLM environment variables.
// Each source is applied in full first; any unset fields are filled from the next source.
func (r *Reconciler) resolveProxyConfiguration(esc *operatorv1alpha1.ExternalSecretsConfig) (*operatorv1alpha1.ProxyConfig, error) {
	proxyConfig := &operatorv1alpha1.ProxyConfig{}
	if esc.Spec.ApplicationConfig.Proxy != nil {
		esc.Spec.ApplicationConfig.Proxy.DeepCopyInto(proxyConfig)
	}

	if r.esm.Spec.GlobalConfig != nil && r.esm.Spec.GlobalConfig.Proxy != nil {
		mergeProxyConfigFields(proxyConfig, r.esm.Spec.GlobalConfig.Proxy)
	}
	mergeProxyConfigFields(proxyConfig, olmProxyConfig())

	if !hasEffectiveProxyURLs(proxyConfig) {
		return nil, nil
	}

	if err := validateProxy(proxyConfig.HTTPProxy); err != nil {
		return nil, fmt.Errorf("failed to validate HTTP proxy: %w", err)
	}
	if err := validateProxy(proxyConfig.HTTPSProxy); err != nil {
		return nil, fmt.Errorf("failed to validate HTTPS proxy: %w", err)
	}

	return proxyConfig, nil
}

func olmProxyConfig() *operatorv1alpha1.ProxyConfig {
	olmHTTPProxy := os.Getenv(httpProxyEnvVar)
	olmHTTPSProxy := os.Getenv(httpsProxyEnvVar)
	olmNoProxy := os.Getenv(noProxyEnvVar)

	if olmHTTPProxy == "" && olmHTTPSProxy == "" && olmNoProxy == "" {
		return nil
	}

	return &operatorv1alpha1.ProxyConfig{
		HTTPProxy:  olmHTTPProxy,
		HTTPSProxy: olmHTTPSProxy,
		NoProxy:    olmNoProxy,
	}
}

func mergeProxyConfigFields(into, from *operatorv1alpha1.ProxyConfig) {
	if into == nil || from == nil {
		return
	}
	if into.HTTPProxy == "" {
		into.HTTPProxy = from.HTTPProxy
	}
	if into.HTTPSProxy == "" {
		into.HTTPSProxy = from.HTTPSProxy
	}
	if into.NoProxy == "" {
		into.NoProxy = from.NoProxy
	}
	if into.NetworkPolicyProvisioning == "" {
		into.NetworkPolicyProvisioning = from.NetworkPolicyProvisioning
	}
}

// hasEffectiveProxyURLs reports whether any proxy URL fields are set.
// A non-nil ProxyConfig with only networkPolicyProvisioning does not count as enabled.
func hasEffectiveProxyURLs(config *operatorv1alpha1.ProxyConfig) bool {
	if config == nil {
		return false
	}
	return config.HTTPProxy != "" || config.HTTPSProxy != "" || config.NoProxy != ""
}

// hasProxyEndpointURLs reports whether HTTP or HTTPS proxy URLs are set.
// NO_PROXY alone does not imply a proxy endpoint for egress NetworkPolicy rules.
func hasProxyEndpointURLs(config *operatorv1alpha1.ProxyConfig) bool {
	if config == nil {
		return false
	}
	return config.HTTPProxy != "" || config.HTTPSProxy != ""
}

// validateProxy checks if the proxy address configured is a valid URL with an explicit
// port in the valid TCP range when one is specified.
func validateProxy(rawURL string) error {
	if rawURL == "" {
		return nil
	}

	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("invalid proxy URL configured")
	}

	// the valid schemes could be http, https, socks5 etc., but we will just ensure
	// scheme and host is not empty.
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("proxy URL must include valid scheme and host")
	}

	if portStr := parsedURL.Port(); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("invalid port %q in proxy URL: %w", portStr, err)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d out of range in proxy URL", port)
		}
	}

	return nil
}

func (r *Reconciler) isProxyEnabled() bool {
	return hasEffectiveProxyURLs(r.proxyConfig)
}

// hasManagedOrWatchLabel reports whether labels identify an operand resource created
// by the operator or a user configured resource that the operator watches.
func hasManagedOrWatchLabel(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	return labels[ManagedResourceLabelKey] == ManagedResourceLabelValue ||
		labels[WatchedResourceLabelKey] == WatchedResourceLabelValue
}

// isManagedResource reports whether object is an operator-managed operand resource.
func isManagedResource(object client.Object) bool {
	if object == nil {
		return false
	}
	labels := object.GetLabels()
	return labels != nil && labels[ManagedResourceLabelKey] == ManagedResourceLabelValue
}

// isManagedOrWatchedResource reports whether object is an operator-managed operand or a
// user-provided resource labeled for passive watch (e.g. trustedCABundle ConfigMaps).
func isManagedOrWatchedResource(object client.Object) bool {
	if object == nil {
		return false
	}
	return hasManagedOrWatchLabel(object.GetLabels())
}

// labelMatchPredicate returns predicate.Funcs that admit events when match returns true for
// the event object(s). On updates both old and new are checked so reconciliation still runs
// when a matching label is removed externally.
func labelMatchPredicate(match func(client.Object) bool) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return match(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return match(e.ObjectOld) || match(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return match(e.Object)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return match(e.Object)
		},
	}
}

// getWithCacheFallback reads a resource from the manager cache first. On IsNotFound it
// falls back to UncachedClient for objects not yet synced into the cache.
func (r *Reconciler) getWithCacheFallback(key types.NamespacedName, obj client.Object) error {
	if err := r.Get(r.ctx, key, obj); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return r.UncachedClient.Get(r.ctx, key, obj)
}

// updateWatchLabel ensures WatchedResourceLabelKey is set on a referenced resource at key
// so subsequent changes enqueue ExternalSecretsConfig reconciliation. obj is an empty instance
// of the resource type to patch. The object is loaded via getWithCacheFallback and the label is
// applied with a merge patch to avoid overwriting unrelated fields and to reduce conflict with
// concurrent writers.
func (r *Reconciler) updateWatchLabel(key types.NamespacedName, obj client.Object) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.getWithCacheFallback(key, obj); err != nil {
			return err
		}
		if labels := obj.GetLabels(); labels != nil && labels[WatchedResourceLabelKey] == WatchedResourceLabelValue {
			return nil
		}

		base := obj.DeepCopyObject().(client.Object)
		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		} else {
			labels = maps.Clone(labels)
		}
		labels[WatchedResourceLabelKey] = WatchedResourceLabelValue
		obj.SetLabels(labels)

		patch := client.MergeFrom(base)
		if err := r.UncachedClient.Patch(r.ctx, obj, patch); err != nil {
			return fmt.Errorf("failed to patch watch label on %q: %w", key, err)
		}
		return nil
	})
}

// createWithFallback attempts to create a resource and handles the AlreadyExists error that
// occurs when the resource exists on the API server but is invisible to the label-filtered
// informer cache (e.g. because the managed label app=external-secrets was externally removed).
//
// When Create returns AlreadyExists, this method bypasses the stale cache by using the
// uncached client to update the resource directly on the API server, restoring the managed
// labels and annotations to the desired state.
//
// It records a "created" event on success, or a "restored" event when the AlreadyExists
// fallback path is taken.
func (r *Reconciler) createWithFallback(desired client.Object, resourceMetadata common.ResourceMetadata, resourceName string, esc *operatorv1alpha1.ExternalSecretsConfig) error {
	kind := desired.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		gvk, gvkErr := apiutil.GVKForObject(desired, r.Scheme)
		if gvkErr != nil {
			r.log.V(5).Info("could not determine GVK, falling back to Go type name",
				"type", fmt.Sprintf("%T", desired), "err", gvkErr)
			kind = fmt.Sprintf("%T", desired)
		} else {
			kind = gvk.Kind
		}
	}

	if err := r.Create(r.ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return common.FromClientError(err, "failed to create %s %s", kind, resourceName)
		}

		r.log.V(1).Info("resource exists on API server but absent from label-filtered cache, restoring desired state",
			"kind", kind, "name", resourceName)
		common.RemoveObsoleteAnnotations(desired, resourceMetadata)
		if err := r.UncachedClient.UpdateWithRetry(r.ctx, desired); err != nil {
			return common.FromClientError(err, "failed to restore %s %s to desired state", kind, resourceName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "%s resource %s restored to desired state", kind, resourceName)
		return nil
	}
	r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "%s resource %s created", kind, resourceName)
	return nil
}

// createWithMetadataFallback attempts to create a resource and handles the AlreadyExists error
// that occurs when the resource exists on the API server but is invisible to the label-filtered
// informer cache. Unlike createWithFallback, only metadata is restored on AlreadyExists so
// externally managed fields (e.g. Secret Data, ConfigMap Data) are left untouched.
func (r *Reconciler) createWithMetadataFallback(desired client.Object, resourceMetadata common.ResourceMetadata, resourceName string, esc *operatorv1alpha1.ExternalSecretsConfig) error {
	kind := desired.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		gvk, gvkErr := apiutil.GVKForObject(desired, r.Scheme)
		if gvkErr != nil {
			r.log.V(5).Info("could not determine GVK, falling back to Go type name",
				"type", fmt.Sprintf("%T", desired), "err", gvkErr)
			kind = fmt.Sprintf("%T", desired)
		} else {
			kind = gvk.Kind
		}
	}

	if err := r.Create(r.ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return common.FromClientError(err, "failed to create %s %s", kind, resourceName)
		}

		r.log.V(1).Info("resource exists on API server but absent from label-filtered cache, patching metadata",
			"kind", kind, "name", resourceName)
		if patchErr := r.patchResourceMetadata(desired, resourceMetadata); patchErr != nil {
			return common.FromClientError(patchErr, "failed to patch %s %s metadata", kind, resourceName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "%s resource %s restored to desired state", kind, resourceName)
		return nil
	}
	r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "%s resource %s created", kind, resourceName)
	return nil
}

type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

// patchResourceMetadata fully replaces the labels and annotations on an object using a
// JSON Patch, leaving all other fields untouched. This is safe for co-managed
// resources where this operator owns all metadata but data fields are owned by an external
// controller (e.g. CNO-managed ConfigMap data, cert-controller-managed TLS Secret data).
//
// JSON Patch "add" on an existing path replaces the entire value, so the resulting
// labels/annotations on the server exactly match desired.
//
// RemoveObsoleteAnnotations is called defensively to strip any deleted annotation keys
// from desired before building the patch, regardless of whether the caller already did so.
func (r *Reconciler) patchResourceMetadata(desired client.Object, resourceMetadata common.ResourceMetadata) error {
	common.RemoveObsoleteAnnotations(desired, resourceMetadata)

	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	labels := desired.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	ops := []jsonPatchOp{
		{Op: "add", Path: "/metadata/labels", Value: labels},
		{Op: "add", Path: "/metadata/annotations", Value: annotations},
	}
	patchBytes, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata patch: %w", err)
	}
	return r.CtrlClient.Patch(r.ctx, desired, client.RawPatch(types.JSONPatchType, patchBytes))
}
