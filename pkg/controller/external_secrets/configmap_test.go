package external_secrets

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/client/fakes"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	"github.com/openshift/external-secrets-operator/pkg/controller/commontest"
)

// testESCWithProxy returns an ExternalSecretsConfig with a proxy configured so that
// ensureTrustedCABundleConfigMap proceeds past the proxy-nil guard.
func testESCWithProxy() *operatorv1alpha1.ExternalSecretsConfig {
	esc := commontest.TestExternalSecretsConfig()
	esc.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
		HTTPProxy: "http://proxy.example.com:3128",
	}
	return esc
}

func TestEnsureTrustedCABundleConfigMap(t *testing.T) {
	cnoData := map[string]string{"ca-bundle.crt": "cert-data"}

	tests := []struct {
		name             string
		resourceMetadata common.ResourceMetadata
		preReq           func(r *Reconciler, cached *fakes.FakeCtrlClient, uncached *fakes.FakeCtrlClient)
		wantErr          string
		wantCreate       bool
		wantPatch        bool
		noProxy          bool
	}{
		{
			name:             "ConfigMap created when it does not exist",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, _ client.Object) (bool, error) {
					return false, nil
				})
				cached.CreateCalls(func(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
					cm, ok := obj.(*corev1.ConfigMap)
					if !ok {
						t.Errorf("expected ConfigMap, got %T", obj)
					}
					if cm.Name != ProxyTrustedCABundleConfigMapName {
						t.Errorf("expected name %s, got %s", ProxyTrustedCABundleConfigMapName, cm.Name)
					}
					if cm.Labels[TrustedCABundleInjectLabel] != "true" {
						t.Errorf("expected inject label to be 'true', got %q", cm.Labels[TrustedCABundleInjectLabel])
					}
					return nil
				})
			},
			wantCreate: true,
		},
		{
			name:             "ConfigMap exists in correct state, no update needed",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, obj client.Object) (bool, error) {
					existing := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name:      ProxyTrustedCABundleConfigMapName,
							Namespace: commontest.TestExternalSecretsNamespace,
							Labels:    getTrustedCABundleLabels(controllerDefaultResourceLabels),
						},
						Data: cnoData,
					}
					existing.DeepCopyInto(obj.(*corev1.ConfigMap))
					return true, nil
				})
			},
		},
		{
			name:             "ConfigMap exists with wrong labels, patch triggered",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, obj client.Object) (bool, error) {
					existing := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name:      ProxyTrustedCABundleConfigMapName,
							Namespace: commontest.TestExternalSecretsNamespace,
							Labels:    map[string]string{"app": "something-else"},
						},
						Data: cnoData,
					}
					existing.DeepCopyInto(obj.(*corev1.ConfigMap))
					return true, nil
				})
				cached.PatchCalls(func(_ context.Context, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
					cm, ok := obj.(*corev1.ConfigMap)
					if !ok {
						t.Errorf("expected ConfigMap, got %T", obj)
					}
					if cm.Labels[TrustedCABundleInjectLabel] != "true" {
						t.Errorf("expected inject label in patch target, got %q", cm.Labels[TrustedCABundleInjectLabel])
					}
					if patch.Type() != types.JSONPatchType {
						t.Errorf("expected JSONPatch, got %s", patch.Type())
					}
					return nil
				})
			},
			wantPatch: true,
		},
		{
			// Regression test for: labels updating with app: external-secrets of cm
			// external-secrets-trusted-ca-bundle will block the reconcile in the http_proxy env.
			//
			// When the managed label (app=external-secrets) is removed from the ConfigMap,
			// the object falls out of the label-filtered cache. The cached Exists() returns
			// false, Create() fails with AlreadyExists, and the controller must patch only
			// the metadata (labels/annotations), leaving CNO-managed Data/BinaryData untouched.
			name:             "Create returns AlreadyExists (label-filtered cache miss): patches metadata to restore labels",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, _ client.Object) (bool, error) {
					return false, nil
				})
				cached.CreateCalls(func(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, ProxyTrustedCABundleConfigMapName)
				})
				cached.PatchCalls(func(_ context.Context, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
					cm, ok := obj.(*corev1.ConfigMap)
					if !ok {
						t.Errorf("expected ConfigMap, got %T", obj)
					}
					if cm.Labels["app"] != "external-secrets" {
						t.Errorf("expected app=external-secrets label in patch target, got %q", cm.Labels["app"])
					}
					if cm.Labels[TrustedCABundleInjectLabel] != "true" {
						t.Errorf("expected inject label in patch target, got %q", cm.Labels[TrustedCABundleInjectLabel])
					}
					if patch.Type() != types.JSONPatchType {
						t.Errorf("expected JSONPatch, got %s", patch.Type())
					}
					return nil
				})
			},
			wantPatch: true,
		},
		{
			name:             "proxy not configured: ConfigMap reconcile skipped",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			noProxy:          true,
		},
		{
			name:             "cached Exists check fails",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, _ client.Object) (bool, error) {
					return false, commontest.ErrTestClient
				})
			},
			wantErr: "failed to check external-secrets/external-secrets-trusted-ca-bundle trusted CA bundle ConfigMap resource already exists: test client error",
		},
		{
			name:             "Create fails with non-AlreadyExists error",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, _ client.Object) (bool, error) {
					return false, nil
				})
				cached.CreateCalls(func(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr: "failed to create external-secrets/external-secrets-trusted-ca-bundle trusted CA bundle ConfigMap resource: test client error",
		},
		{
			name:             "Patch fails after AlreadyExists from Create",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, _ client.Object) (bool, error) {
					return false, nil
				})
				cached.CreateCalls(func(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, ProxyTrustedCABundleConfigMapName)
				})
				cached.PatchCalls(func(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr: "failed to patch external-secrets/external-secrets-trusted-ca-bundle trusted CA bundle ConfigMap metadata: test client error",
		},
		{
			name:             "cached Patch fails when ConfigMap has wrong labels",
			resourceMetadata: testResourceMetadata(commontest.TestExternalSecretsConfig()),
			preReq: func(_ *Reconciler, cached *fakes.FakeCtrlClient, _ *fakes.FakeCtrlClient) {
				cached.ExistsCalls(func(_ context.Context, _ types.NamespacedName, obj client.Object) (bool, error) {
					existing := &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name:      ProxyTrustedCABundleConfigMapName,
							Namespace: commontest.TestExternalSecretsNamespace,
							Labels:    nil,
						},
					}
					existing.DeepCopyInto(obj.(*corev1.ConfigMap))
					return true, nil
				})
				cached.PatchCalls(func(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr: "failed to patch external-secrets/external-secrets-trusted-ca-bundle trusted CA bundle ConfigMap metadata: test client error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			cached := &fakes.FakeCtrlClient{}
			uncached := &fakes.FakeCtrlClient{}
			if tt.preReq != nil {
				tt.preReq(r, cached, uncached)
			}
			r.CtrlClient = cached
			r.UncachedClient = uncached

			var esc *operatorv1alpha1.ExternalSecretsConfig
			if tt.noProxy {
				esc = commontest.TestExternalSecretsConfig()
			} else {
				esc = testESCWithProxy()
			}
			if proxyConfig, err := r.resolveProxyConfiguration(esc); err != nil {
				t.Fatalf("resolveProxyConfiguration() error = %v", err)
			} else {
				r.proxyConfig = proxyConfig
			}

			err := r.ensureTrustedCABundleConfigMap(esc, tt.resourceMetadata)

			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("ensureTrustedCABundleConfigMap() err = %v, wantErr = %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Errorf("ensureTrustedCABundleConfigMap() unexpected error: %v", err)
			}

			if tt.wantCreate && cached.CreateCallCount() == 0 {
				t.Error("expected Create to be called, but it wasn't")
			}
			if tt.wantPatch && cached.PatchCallCount() == 0 {
				t.Error("expected Patch to be called, but it wasn't")
			}
			if !tt.wantPatch && cached.PatchCallCount() > 0 {
				t.Error("expected no Patch call, but one was made")
			}
		})
	}
}
