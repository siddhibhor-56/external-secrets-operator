package external_secrets

import (
	"context"
	"testing"

	webhook "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr/testr"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/client/fakes"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	"github.com/openshift/external-secrets-operator/pkg/controller/commontest"
	"github.com/openshift/external-secrets-operator/pkg/operator/assets"
)

// testResourceMetadata returns a ResourceMetadata with the default labels and
// annotations from the given ExternalSecretsConfig.
func testResourceMetadata(esc *operatorv1alpha1.ExternalSecretsConfig) common.ResourceMetadata {
	return common.ResourceMetadata{
		Labels:                controllerDefaultResourceLabels,
		Annotations:           esc.Spec.ControllerConfig.Annotations,
		DeletedAnnotationKeys: []string{},
	}
}

// testReconciler returns a sample Reconciler instance.
func testReconciler(t *testing.T) *Reconciler {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(certmanagerv1.AddToScheme(scheme))

	return &Reconciler{
		Scheme:                scheme,
		ctx:                   context.Background(),
		eventRecorder:         record.NewFakeRecorder(100),
		log:                   testr.New(t),
		esm:                   commontest.TestExternalSecretsManager(),
		optionalResourcesList: make(map[string]struct{}),
		now:                   &common.Now{},
	}
}

// testService returns a Service object decoded from the specified asset file.
func testService(assetName string) *corev1.Service {
	service := common.DecodeServiceObjBytes(assets.MustAsset(assetName))
	service.SetLabels(controllerDefaultResourceLabels)
	return service
}

// testServiceAccount returns a ServiceAccount object decoded from the specified asset file.
func testServiceAccount(assetName string) *corev1.ServiceAccount {
	serviceAccount := common.DecodeServiceAccountObjBytes(assets.MustAsset(assetName))
	serviceAccount.SetLabels(controllerDefaultResourceLabels)
	return serviceAccount
}

// testClusterRole returns ClusterRole object read from provided static asset of same kind.
func testClusterRole(assetName string) *rbacv1.ClusterRole {
	role := common.DecodeClusterRoleObjBytes(assets.MustAsset(assetName))
	role.SetLabels(controllerDefaultResourceLabels)
	return role
}

// testClusterRoleBinding returns ClusterRoleBinding object read from provided static asset of same kind.
func testClusterRoleBinding(assetName string) *rbacv1.ClusterRoleBinding {
	roleBinding := common.DecodeClusterRoleBindingObjBytes(assets.MustAsset(assetName))
	roleBinding.SetLabels(controllerDefaultResourceLabels)
	return roleBinding
}

// testRole returns Role object read from provided static asset of same kind.
func testRole(assetName string) *rbacv1.Role {
	role := common.DecodeRoleObjBytes(assets.MustAsset(assetName))
	role.SetLabels(controllerDefaultResourceLabels)
	return role
}

// testRoleBinding returns RoleBinding object read from provided static asset of same kind.
func testRoleBinding(assetName string) *rbacv1.RoleBinding {
	roleBinding := common.DecodeRoleBindingObjBytes(assets.MustAsset(assetName))
	roleBinding.SetLabels(controllerDefaultResourceLabels)
	return roleBinding
}

// testValidatingWebhookConfiguration returns ValidatingWebhookConfiguration object read from provided static asset of same kind.
func testValidatingWebhookConfiguration(assetName string) *webhook.ValidatingWebhookConfiguration {
	validateWebhook := common.DecodeValidatingWebhookConfigurationObjBytes(assets.MustAsset(assetName))
	return validateWebhook
}

// Helper function to create a dummy deployment for testing.
func testDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: controllerDefaultResourceLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: controllerDefaultResourceLabels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  externalsecretsCommonName,
							Image: commontest.TestExternalSecretsImageName,
						},
					},
				},
			},
		},
	}
}

// testCertificate returns Certificate object read from provided static asset of same kind.
func testCertificate(assetName string) *certmanagerv1.Certificate {
	validateCertificate := common.DecodeCertificateObjBytes(assets.MustAsset(assetName))
	validateCertificate.SetLabels(controllerDefaultResourceLabels)
	return validateCertificate
}

// testSecret returns Secret object read from provided static asset of same kind.
//
//nolint:unparam // assetName kept as parameter for future test scenarios, thought currently just webhookTLSSecretAssetName is passed.
func testSecret(assetName string) *corev1.Secret {
	validateSecret := common.DecodeSecretObjBytes(assets.MustAsset(assetName))
	validateSecret.SetLabels(controllerDefaultResourceLabels)
	return validateSecret
}

// testNetworkPolicy returns NetworkPolicy object read from provided static asset of same kind.
func testNetworkPolicy(assetName string) *networkingv1.NetworkPolicy {
	networkPolicy := common.DecodeNetworkPolicyObjBytes(assets.MustAsset(assetName))
	networkPolicy.SetLabels(controllerDefaultResourceLabels)
	return networkPolicy
}

func testUserCAConfigMap(name string, pem string, labels map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: OperandDefaultNamespace,
			Labels:    labels,
		},
		Data: map[string]string{
			UserCABundleKeyPath: pem,
		},
	}
}

func setupConfigMapClients(t *testing.T, cm *corev1.ConfigMap) (*fakes.FakeCtrlClient, *fakes.FakeCtrlClient) {
	t.Helper()
	cached := &fakes.FakeCtrlClient{}
	uncached := &fakes.FakeCtrlClient{}
	stubGet := func(_ context.Context, ns types.NamespacedName, obj client.Object) error {
		if ns != client.ObjectKeyFromObject(cm) {
			return apierrors.NewNotFound(corev1.Resource("configmaps"), ns.Name)
		}
		cm.DeepCopyInto(obj.(*corev1.ConfigMap))
		return nil
	}
	cached.GetCalls(stubGet)
	uncached.GetCalls(stubGet)
	uncached.PatchCalls(func(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
		return nil
	})
	return cached, uncached
}
