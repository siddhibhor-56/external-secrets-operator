package external_secrets

import (
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
)

// ensureTrustedCABundleConfigMap creates or ensures the trusted CA bundle ConfigMap exists
// in the operand namespace when proxy configuration is present. The ConfigMap is labeled
// with the injection label required by the Cluster Network Operator (CNO), which watches
// for this label and injects the cluster's trusted CA bundle into the ConfigMap's data.
// This function ensures the correct labels are present so that CNO can manage the CA bundle
// content as expected.
func (r *Reconciler) ensureTrustedCABundleConfigMap(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata) error {
	// Only create ConfigMap if proxy is configured
	if !r.isProxyEnabled() {
		// TODO: ConfigMap removal when proxy configuration is removed
		// will be revisited in a follow-up implementation.
		r.log.V(4).Info("no proxy configuration found, skipping trusted CA bundle ConfigMap creation")
		return nil
	}

	namespace := getNamespace(esc)
	expectedLabels := getTrustedCABundleLabels(resourceMetadata.Labels)

	desiredConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProxyTrustedCABundleConfigMapName,
			Namespace: namespace,
			Labels:    expectedLabels,
		},
	}

	// Apply managed annotations from ResourceMetadataAnnotations
	common.ApplyResourceMetadata(desiredConfigMap, resourceMetadata)

	configMapName := fmt.Sprintf("%s/%s", desiredConfigMap.GetNamespace(), desiredConfigMap.GetName())
	r.log.V(4).Info("reconciling trusted CA bundle ConfigMap resource", "name", configMapName)

	// Check if the ConfigMap already exists
	existingConfigMap := &corev1.ConfigMap{}
	exist, err := r.Exists(r.ctx, client.ObjectKeyFromObject(desiredConfigMap), existingConfigMap)
	if err != nil {
		return common.FromClientError(err, "failed to check %s trusted CA bundle ConfigMap resource already exists", configMapName)
	}

	if !exist {
		// NOTE: This ConfigMap cannot use the generic createWithFallback helper because
		// the Data/BinaryData fields are managed by CNO (not by this operator). On
		// AlreadyExists we use a MergePatch that touches only metadata, leaving
		// CNO-injected CA certificates untouched.
		if err := r.Create(r.ctx, desiredConfigMap); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return common.FromClientError(err, "failed to create %s trusted CA bundle ConfigMap resource", configMapName)
			}
			r.log.V(1).Info("trusted CA bundle ConfigMap exists on API server but absent from label-filtered cache, patching metadata", "name", configMapName)
			if patchErr := r.patchResourceMetadata(desiredConfigMap, resourceMetadata); patchErr != nil {
				return common.FromClientError(patchErr, "failed to patch %s trusted CA bundle ConfigMap metadata", configMapName)
			}
			r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "trusted CA bundle ConfigMap resource %s restored to desired state", configMapName)
			return nil
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "trusted CA bundle ConfigMap resource %s created", configMapName)
		return nil
	}

	// ConfigMap exists — ensure it has the correct labels and annotations.
	// Do not update the data of the ConfigMap since it is managed by CNO.
	// MergeFetchedAnnotations preserves external annotations (e.g., CNO's openshift.io/owning-component).
	// Use a metadata-only patch so CNO-managed Data/BinaryData are never touched.
	if common.ObjectMetadataModified(desiredConfigMap, existingConfigMap, &resourceMetadata) {
		r.log.V(1).Info("trusted CA bundle ConfigMap has been modified, patching metadata to desired state", "name", configMapName)
		if err := r.patchResourceMetadata(desiredConfigMap, resourceMetadata); err != nil {
			return common.FromClientError(err, "failed to patch %s trusted CA bundle ConfigMap metadata", configMapName)
		}
		r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "trusted CA bundle ConfigMap resource %s reconciled back to desired state", configMapName)
	} else {
		r.log.V(4).Info("trusted CA bundle ConfigMap resource already exists and is in expected state", "name", configMapName)
	}

	return nil
}

// getTrustedCABundleLabels merges resource labels with the injection label.
func getTrustedCABundleLabels(resourceLabels map[string]string) map[string]string {
	labels := make(map[string]string)
	maps.Copy(labels, resourceLabels)
	labels[TrustedCABundleInjectLabel] = "true"
	return labels
}
