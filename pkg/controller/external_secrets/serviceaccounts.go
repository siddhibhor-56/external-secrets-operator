package external_secrets

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	"github.com/openshift/external-secrets-operator/pkg/operator/assets"
)

// createOrApplyServiceAccounts ensures required service Account resources exist and are correctly configured.
func (r *Reconciler) createOrApplyServiceAccounts(esc *operatorv1alpha1.ExternalSecretsConfig, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	serviceAccountsToCreate := []struct {
		assetName string
		condition bool
	}{
		{
			assetName: controllerServiceAccountAssetName,
			condition: true,
		},
		{
			assetName: webhookServiceAccountAssetName,
			condition: true,
		},
		{
			assetName: certControllerServiceAccountAssetName,
			condition: !isCertManagerConfigEnabled(esc),
		},
		{
			assetName: bitwardenServiceAccountAssetName,
			condition: isBitwardenConfigEnabled(esc),
		},
	}

	for _, serviceAccount := range serviceAccountsToCreate {
		if !serviceAccount.condition {
			continue
		}
		if err := r.createOrApplyServiceAccountFromAsset(esc, serviceAccount.assetName, resourceMetadata, externalSecretsConfigCreateRecon); err != nil {
			return err
		}
	}

	return nil
}

func (r *Reconciler) createOrApplyServiceAccountFromAsset(esc *operatorv1alpha1.ExternalSecretsConfig, assetName string, resourceMetadata common.ResourceMetadata, externalSecretsConfigCreateRecon bool) error {
	desired := common.DecodeServiceAccountObjBytes(assets.MustAsset(assetName))
	updateNamespace(desired, esc)
	common.ApplyResourceMetadata(desired, resourceMetadata)

	serviceAccountName := fmt.Sprintf("%s/%s", desired.GetNamespace(), desired.GetName())
	r.log.V(4).Info("reconciling serviceaccount resource", "name", serviceAccountName)

	fetched := &corev1.ServiceAccount{}
	exist, err := r.Exists(r.ctx, client.ObjectKeyFromObject(desired), fetched)
	if err != nil {
		return common.FromClientError(err, "failed to check if serviceaccount %s exists", serviceAccountName)
	}

	if !exist {
		return r.createWithFallback(desired, resourceMetadata, serviceAccountName, esc)
	}

	if externalSecretsConfigCreateRecon {
		r.eventRecorder.Eventf(esc, corev1.EventTypeWarning, "ResourceAlreadyExists", "%s serviceaccount already exists, possibly from a previous install", serviceAccountName)
	}

	if !common.HasObjectChanged(desired, fetched, &resourceMetadata) {
		r.log.V(4).Info("serviceaccount already up-to-date", "name", serviceAccountName)
		return nil
	}

	r.log.V(1).Info("ServiceAccount modified, updating", "name", serviceAccountName)
	common.RemoveObsoleteAnnotations(desired, resourceMetadata)
	if err := r.UpdateWithRetry(r.ctx, desired); err != nil {
		return common.FromClientError(err, "failed to update serviceaccount %s", serviceAccountName)
	}
	r.eventRecorder.Eventf(esc, corev1.EventTypeNormal, "Reconciled", "ServiceAccount %s updated", serviceAccountName)

	return nil
}
