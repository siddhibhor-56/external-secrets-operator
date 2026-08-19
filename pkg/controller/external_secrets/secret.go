package external_secrets

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	"github.com/openshift/external-secrets-operator/pkg/operator/assets"
)

func (r *Reconciler) createOrApplySecret(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, recon bool) error {
	// secrets are only created if isCertManagerConfig is not enabled
	if isCertManagerConfigEnabled(esc) {
		r.log.V(4).Info("cert-manager config is enabled, skipping webhook component secret resource creation")
		return nil
	}

	desired := r.getSecretObject(esc, resourceMetadata)
	secretName := fmt.Sprintf("%s/%s", desired.GetNamespace(), desired.GetName())
	r.log.V(4).Info("reconciling secret resource", "name", secretName)
	fetched := &corev1.Secret{}

	exist, err := r.Exists(r.ctx, client.ObjectKeyFromObject(desired), fetched)
	if err != nil {
		return common.FromClientError(err, "failed to check %s secret resource already exists", secretName)
	}

	if !exist {
		// NOTE: This Secret cannot use the generic createWithFallback helper because
		// its Data field is managed by the external-secrets cert-controller, which injects
		// TLS content at runtime. On AlreadyExists we use a JSON patch that touches only
		// metadata, leaving cert-controller-managed TLS certificates untouched.
		return r.createWithMetadataFallback(desired, resourceMetadata, secretName, esc)
	}

	if recon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "%s secret resource already exists, maybe from previous installation", secretName)
	}

	if !common.ObjectMetadataModified(desired, fetched, &resourceMetadata) {
		r.log.V(4).Info("secret resource already exists and is in expected state", "name", secretName)
		return nil
	}

	r.log.V(1).Info("secret has been modified, patching metadata to desired state", "name", secretName)
	if err := r.patchResourceMetadata(desired, resourceMetadata); err != nil {
		return common.FromClientError(err, "failed to patch Secret %s metadata", secretName)
	}
	r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "secret resource %s reconciled back to desired state", secretName)

	return nil
}

func (r *Reconciler) getSecretObject(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata) *corev1.Secret {
	secret := common.DecodeSecretObjBytes(assets.MustAsset(webhookTLSSecretAssetName))

	updateNamespace(secret, esc)
	common.ApplyResourceMetadata(secret, resourceMetadata)

	return secret
}
