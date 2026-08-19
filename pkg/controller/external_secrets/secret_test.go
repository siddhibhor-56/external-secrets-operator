package external_secrets

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/client/fakes"
	"github.com/openshift/external-secrets-operator/pkg/controller/commontest"
)

const (
	testValidateSecretResourceName = "external-secrets-webhook"
)

func TestCreateOrApplySecret(t *testing.T) {
	tests := []struct {
		name    string
		preReq  func(*Reconciler, *fakes.FakeCtrlClient)
		esc     func(*v1alpha1.ExternalSecretsConfig)
		wantErr string
	}{
		{
			name:   "external secret spec disabled",
			preReq: nil,
			esc: func(esc *v1alpha1.ExternalSecretsConfig) {
				esc.Spec = v1alpha1.ExternalSecretsConfigSpec{}
			},
		},
		{
			name:   "webhook config is nil",
			preReq: nil,
			esc: func(esc *v1alpha1.ExternalSecretsConfig) {
				esc.Spec = v1alpha1.ExternalSecretsConfigSpec{
					ApplicationConfig: v1alpha1.ApplicationConfig{
						WebhookConfig: nil,
					},
				}
			},
		},
		{
			name:   "webhook config is empty",
			preReq: nil,
			esc: func(esc *v1alpha1.ExternalSecretsConfig) {
				esc.Spec = v1alpha1.ExternalSecretsConfigSpec{
					ApplicationConfig: v1alpha1.ApplicationConfig{
						WebhookConfig: &v1alpha1.WebhookConfig{},
					},
				}
			},
		},
		{
			name:   "cert manager config is nil",
			preReq: nil,
			esc: func(esc *v1alpha1.ExternalSecretsConfig) {
				esc.Spec = v1alpha1.ExternalSecretsConfigSpec{
					ControllerConfig: v1alpha1.ControllerConfig{
						CertProvider: &v1alpha1.CertProvidersConfig{
							CertManager: nil,
						},
					},
				}
			},
		},
		{
			name: "reconciliation of secret fails while checking if exists",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.GetCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) error {
					if o, ok := obj.(*corev1.Secret); ok {
						secret := testSecret(webhookTLSSecretAssetName)
						secret.DeepCopyInto(o)
					}
					return nil
				})
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if _, ok := obj.(*corev1.Secret); ok {
						return false, commontest.ErrTestClient
					}
					return true, nil
				})
			},
			wantErr: fmt.Sprintf("failed to check %s/%s secret resource already exists: %s", commontest.TestExternalSecretsNamespace, testValidateSecretResourceName, commontest.ErrTestClient),
		},
		{
			name: "reconciliation of secret fails while patching metadata to expected state",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.GetCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) error {
					if o, ok := obj.(*corev1.Secret); ok {
						secret := testSecret(webhookTLSSecretAssetName)
						secret.DeepCopyInto(o)
					}
					return nil
				})
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if o, ok := obj.(*corev1.Secret); ok {
						secret := testSecret(webhookTLSSecretAssetName)
						secret.SetLabels(map[string]string{"test": "test"})
						secret.DeepCopyInto(o)
					}
					return true, nil
				})
				m.PatchCalls(func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						return commontest.ErrTestClient
					}
					return nil
				})
			},
			wantErr: fmt.Sprintf("failed to patch Secret %s/%s metadata: %s", commontest.TestExternalSecretsNamespace, testValidateSecretResourceName, commontest.ErrTestClient),
		},
		{
			// Regression test: when the managed label is removed from the Secret, the object
			// falls out of the label-filtered cache. cached Exists() returns false, Create()
			// fails with AlreadyExists, and the controller must patch only the metadata,
			// leaving cert-controller-injected TLS data untouched.
			name: "Create returns AlreadyExists (label-filtered cache miss): patches metadata to restore labels",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, testValidateSecretResourceName)
				})
				m.PatchCalls(func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						if obj.GetLabels()["app"] != "external-secrets" {
							t.Errorf("expected app=external-secrets label in patch target, got %v", obj.GetLabels())
						}
					}
					return nil
				})
			},
		},
		{
			name: "Patch fails after AlreadyExists from Create",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, testValidateSecretResourceName)
				})
				m.PatchCalls(func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr: fmt.Sprintf("failed to patch Secret %s/%s metadata: %s", commontest.TestExternalSecretsNamespace, testValidateSecretResourceName, commontest.ErrTestClient),
		},
		{
			name: "reconciliation of secret which already exists in expected state",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.GetCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) error {
					if o, ok := obj.(*corev1.Secret); ok {
						secret := testSecret(webhookTLSSecretAssetName)
						secret.DeepCopyInto(o)
					}
					return nil
				})
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if o, ok := obj.(*corev1.Secret); ok {
						secret := testSecret(webhookTLSSecretAssetName)
						secret.DeepCopyInto(o)
					}
					return true, nil
				})
			},
		},
		{
			name: "reconciliation of secret creation fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.GetCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) error {
					if o, ok := obj.(*corev1.Secret); ok {
						secret := testSecret(webhookTLSSecretAssetName)
						secret.DeepCopyInto(o)
					}
					return nil
				})
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if _, ok := obj.(*corev1.Secret); ok {
						return false, nil
					}
					return true, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*corev1.Secret); ok {
						return commontest.ErrTestClient
					}
					return nil
				})
			},
			wantErr: fmt.Sprintf("failed to create Secret %s/%s: %s", commontest.TestExternalSecretsNamespace, testValidateSecretResourceName, commontest.ErrTestClient),
		},
		{
			name: "successful secret creation",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})

				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return nil
				})
			},
		},
		{
			name: "secret creation skipped when cert-manager config is enabled",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
			},
		},
		{
			name: "secret with custom annotations applied successfully",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if secret, ok := obj.(*corev1.Secret); ok {
						// Verify annotations are applied
						if secret.Annotations == nil {
							t.Error("secret annotations should not be nil")
							return nil
						}
						if secret.Annotations["vault.hashicorp.com/secret-type"] != "webhook-cert" {
							t.Errorf("expected annotation 'vault.hashicorp.com/secret-type'='webhook-cert', got '%s'",
								secret.Annotations["vault.hashicorp.com/secret-type"])
						}
					}
					return nil
				})
			},
			esc: func(esc *v1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.Annotations = map[string]string{
					"vault.hashicorp.com/secret-type": "webhook-cert",
					"security/classification":         "confidential",
				}
			},
		},
		{
			name: "secret tracks managed annotations",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if secret, ok := obj.(*corev1.Secret); ok {
						// Verify all annotations from spec are present
						if secret.Annotations["allowed-secret-annotation"] != "value" {
							t.Errorf("expected 'allowed-secret-annotation'")
						}
					}
					return nil
				})
			},
			esc: func(esc *v1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.Annotations = map[string]string{
					"allowed-secret-annotation": "value",
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			mock := &fakes.FakeCtrlClient{}
			if tt.preReq != nil {
				tt.preReq(r, mock)
			}
			r.CtrlClient = mock
			esc := testExternalSecretsConfigForSecrets()
			if tt.esc != nil {
				tt.esc(esc)
			}

			err := r.createOrApplySecret(esc, testResourceMetadata(esc), false)
			if (tt.wantErr != "" || err != nil) && (err == nil || err.Error() != tt.wantErr) {
				t.Errorf("createOrApplySecret() err: %v, wantErr: %v", err, tt.wantErr)
			}
		})
	}
}

func testExternalSecretsConfigForSecrets() *v1alpha1.ExternalSecretsConfig {
	esc := commontest.TestExternalSecretsConfig()

	esc.Spec = v1alpha1.ExternalSecretsConfigSpec{
		ControllerConfig: v1alpha1.ControllerConfig{
			CertProvider: &v1alpha1.CertProvidersConfig{
				CertManager: &v1alpha1.CertManagerConfig{
					Mode: v1alpha1.Disabled,
				},
			},
		},
		ApplicationConfig: v1alpha1.ApplicationConfig{},
	}
	return esc
}
