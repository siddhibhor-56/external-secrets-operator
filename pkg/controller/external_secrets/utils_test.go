package external_secrets

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/client/fakes"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
)

func TestHasEffectiveProxyURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config *operatorv1alpha1.ProxyConfig
		want   bool
	}{
		{
			name:   "nil config",
			config: nil,
			want:   false,
		},
		{
			name:   "only networkPolicyProvisioning",
			config: &operatorv1alpha1.ProxyConfig{NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateUnmanaged},
			want:   false,
		},
		{
			name:   "http proxy set",
			config: &operatorv1alpha1.ProxyConfig{HTTPProxy: "http://proxy:8080"},
			want:   true,
		},
		{
			name:   "noProxy only",
			config: &operatorv1alpha1.ProxyConfig{NoProxy: "localhost"},
			want:   true,
		},
		{
			name: "all proxy config present",
			config: &operatorv1alpha1.ProxyConfig{
				NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateManaged,
				NoProxy:                   "localhost",
				HTTPProxy:                 "http://proxy:8080",
				HTTPSProxy:                "https://proxy:443",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasEffectiveProxyURLs(tt.config); got != tt.want {
				t.Fatalf("hasEffectiveProxyURLs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasProxyEndpointURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config *operatorv1alpha1.ProxyConfig
		want   bool
	}{
		{
			name:   "nil config",
			config: nil,
			want:   false,
		},
		{
			name:   "only noProxy",
			config: &operatorv1alpha1.ProxyConfig{NoProxy: "localhost"},
			want:   false,
		},
		{
			name:   "http proxy set",
			config: &operatorv1alpha1.ProxyConfig{HTTPProxy: "http://proxy:8080"},
			want:   true,
		},
		{
			name:   "https proxy set",
			config: &operatorv1alpha1.ProxyConfig{HTTPSProxy: "https://proxy:443"},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasProxyEndpointURLs(tt.config); got != tt.want {
				t.Fatalf("hasProxyEndpointURLs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateProxy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{
			name:   "empty URL skipped",
			rawURL: "",
		},
		{
			name:   "valid HTTP URL",
			rawURL: "http://proxy.example.com:8080",
		},
		{
			name:    "invalid URL",
			rawURL:  "not-a-url",
			wantErr: true,
		},
		{
			name:    "invalid port",
			rawURL:  "http://proxy.example.com:abc",
			wantErr: true,
		},
		{
			name:    "port out of range",
			rawURL:  "http://proxy.example.com:70000",
			wantErr: true,
		},
		{
			name:    "port zero",
			rawURL:  "http://proxy.example.com:0",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateProxy(tt.rawURL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateProxy(%q) error = %v, wantErr %v", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestValidateExternalSecretsConfigProxy(t *testing.T) {
	tests := []struct {
		name                 string
		olmHTTPProxy         string
		proxy                *operatorv1alpha1.ProxyConfig
		certManagerEnabled   bool
		wantErr              bool
		wantUserConfigErr    bool
		wantIrrecoverableErr bool
		checkProxyEnabled    bool
		wantProxyEnabled     bool
	}{
		{
			name:         "networkPolicyProvisioning Unmanaged merges OLM proxy URLs for operand",
			olmHTTPProxy: "http://olm-proxy:8080",
			proxy: &operatorv1alpha1.ProxyConfig{
				NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateUnmanaged,
			},
			checkProxyEnabled: true,
			wantProxyEnabled:  true,
		},
		{
			name:         "networkPolicyProvisioning Managed merges OLM proxy URLs",
			olmHTTPProxy: "http://olm-proxy:8080",
			proxy: &operatorv1alpha1.ProxyConfig{
				NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateManaged,
			},
			checkProxyEnabled: true,
			wantProxyEnabled:  true,
		},
		{
			name: "HTTPProxy configured enables proxy",
			proxy: &operatorv1alpha1.ProxyConfig{
				HTTPProxy: "http://proxy:8080",
			},
			checkProxyEnabled: true,
			wantProxyEnabled:  true,
		},
		{
			name: "invalid HTTPProxy returns UserConfigurationError",
			proxy: &operatorv1alpha1.ProxyConfig{
				HTTPProxy: "not-a-url",
			},
			wantErr:           true,
			wantUserConfigErr: true,
		},
		{
			name: "HTTPProxy port out of range returns UserConfigurationError",
			proxy: &operatorv1alpha1.ProxyConfig{
				HTTPProxy: "http://proxy.example.com:70000",
			},
			wantErr:           true,
			wantUserConfigErr: true,
		},
		{
			name: "invalid HTTPSProxy returns UserConfigurationError",
			proxy: &operatorv1alpha1.ProxyConfig{
				HTTPSProxy: "http://proxy.example.com:abc",
			},
			wantErr:           true,
			wantUserConfigErr: true,
		},
		{
			name:                 "cert-manager enabled but not installed returns IrrecoverableError",
			certManagerEnabled:   true,
			wantErr:              true,
			wantIrrecoverableErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{
				httpProxyEnvVar, httpsProxyEnvVar, noProxyEnvVar,
				httpProxyEnvVarLowercase, httpsProxyEnvVarLowercase, noProxyEnvVarLowercase,
			} {
				t.Setenv(key, "")
			}
			if tt.olmHTTPProxy != "" {
				t.Setenv(httpProxyEnvVar, tt.olmHTTPProxy)
			}

			r := &Reconciler{
				esm:                   &operatorv1alpha1.ExternalSecretsManager{},
				optionalResourcesList: map[string]struct{}{},
			}

			esc := &operatorv1alpha1.ExternalSecretsConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			}
			if tt.certManagerEnabled {
				esc.Spec.ControllerConfig.CertProvider = &operatorv1alpha1.CertProvidersConfig{
					CertManager: &operatorv1alpha1.CertManagerConfig{Mode: operatorv1alpha1.Enabled},
				}
			} else {
				esc.Spec.ApplicationConfig.Proxy = tt.proxy
			}

			err := r.validateExternalSecretsConfig(esc)
			if tt.wantErr {
				if err == nil {
					t.Fatal("validateExternalSecretsConfig() error = nil, want error")
				}
				if tt.wantUserConfigErr && !common.IsUserConfigurationError(err) {
					t.Fatalf("validateExternalSecretsConfig() error = %v, want user configuration error", err)
				}
				if tt.wantIrrecoverableErr && !common.IsIrrecoverableError(err) {
					t.Fatalf("validateExternalSecretsConfig() error = %v, want irrecoverable error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateExternalSecretsConfig() error = %v, want nil", err)
			}
			if tt.checkProxyEnabled && r.isProxyEnabled() != tt.wantProxyEnabled {
				t.Fatalf("isProxyEnabled() = %v, want %v", r.isProxyEnabled(), tt.wantProxyEnabled)
			}
		})
	}
}

func TestGetWithCacheFallback(t *testing.T) {
	t.Parallel()

	key := types.NamespacedName{Name: "user-ca", Namespace: OperandDefaultNamespace}
	cm := testUserCAConfigMap("user-ca", mustPEMCert(t, true), nil)
	cacheUnavailableErr := errors.New("cache unavailable")

	tests := []struct {
		name         string
		setupClients func(t *testing.T) (*fakes.FakeCtrlClient, *fakes.FakeCtrlClient)
		wantErr      error
		assertGot    func(t *testing.T, got *corev1.ConfigMap)
	}{
		{
			name: "cache hit",
			setupClients: func(t *testing.T) (*fakes.FakeCtrlClient, *fakes.FakeCtrlClient) {
				t.Helper()
				cached := &fakes.FakeCtrlClient{}
				uncached := &fakes.FakeCtrlClient{}
				cached.GetCalls(func(_ context.Context, _ types.NamespacedName, obj client.Object) error {
					cm.DeepCopyInto(obj.(*corev1.ConfigMap))
					return nil
				})
				uncached.GetCalls(func(context.Context, types.NamespacedName, client.Object) error {
					t.Fatal("uncached Get should not be called when cache hits")
					return nil
				})
				return cached, uncached
			},
			assertGot: func(t *testing.T, got *corev1.ConfigMap) {
				t.Helper()
				if got.Name != cm.Name {
					t.Fatalf("got ConfigMap name %q, want %q", got.Name, cm.Name)
				}
			},
		},
		{
			name: "cache miss falls back to uncached",
			setupClients: func(t *testing.T) (*fakes.FakeCtrlClient, *fakes.FakeCtrlClient) {
				t.Helper()
				cached := &fakes.FakeCtrlClient{}
				uncached := &fakes.FakeCtrlClient{}
				cached.GetReturns(apierrors.NewNotFound(corev1.Resource("configmaps"), key.Name))
				uncached.GetCalls(func(_ context.Context, _ types.NamespacedName, obj client.Object) error {
					cm.DeepCopyInto(obj.(*corev1.ConfigMap))
					return nil
				})
				return cached, uncached
			},
			assertGot: func(t *testing.T, got *corev1.ConfigMap) {
				t.Helper()
				if got.Data[UserCABundleKeyPath] == "" {
					t.Fatal("expected ConfigMap data from uncached client")
				}
			},
		},
		{
			name: "non-not-found cache error",
			setupClients: func(t *testing.T) (*fakes.FakeCtrlClient, *fakes.FakeCtrlClient) {
				t.Helper()
				cached := &fakes.FakeCtrlClient{}
				uncached := &fakes.FakeCtrlClient{}
				cached.GetReturns(cacheUnavailableErr)
				uncached.GetCalls(func(context.Context, types.NamespacedName, client.Object) error {
					t.Fatal("uncached Get should not be called on non-NotFound cache errors")
					return nil
				})
				return cached, uncached
			},
			wantErr: cacheUnavailableErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cached, uncached := tt.setupClients(t)
			r := &Reconciler{ctx: context.Background(), CtrlClient: cached, UncachedClient: uncached}
			got := &corev1.ConfigMap{}
			err := r.getWithCacheFallback(key, got)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("getWithCacheFallback() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("getWithCacheFallback() error = %v", err)
			}
			if tt.assertGot != nil {
				tt.assertGot(t, got)
			}
		})
	}
}

func TestUpdateWatchLabel(t *testing.T) {
	t.Parallel()

	key := types.NamespacedName{Name: "user-ca", Namespace: OperandDefaultNamespace}
	baseCM := testUserCAConfigMap("user-ca", mustPEMCert(t, true), nil)
	labeledCM := testUserCAConfigMap("user-ca", mustPEMCert(t, true), map[string]string{
		WatchedResourceLabelKey: WatchedResourceLabelValue,
	})

	tests := []struct {
		name        string
		cm          *corev1.ConfigMap
		wantPatch   bool
		assertPatch func(t *testing.T, obj client.Object)
	}{
		{
			name:      "patches watch label",
			cm:        baseCM,
			wantPatch: true,
			assertPatch: func(t *testing.T, obj client.Object) {
				t.Helper()
				labels := obj.GetLabels()
				if labels[WatchedResourceLabelKey] != WatchedResourceLabelValue {
					t.Fatalf("patch labels = %v, want watch label set", labels)
				}
			},
		},
		{
			name:      "skips patch when label already set",
			cm:        labeledCM,
			wantPatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cached := &fakes.FakeCtrlClient{}
			uncached := &fakes.FakeCtrlClient{}
			stubGet := func(_ context.Context, _ types.NamespacedName, obj client.Object) error {
				tt.cm.DeepCopyInto(obj.(*corev1.ConfigMap))
				return nil
			}
			cached.GetCalls(stubGet)
			uncached.GetCalls(stubGet)

			var patched bool
			uncached.PatchCalls(func(_ context.Context, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				if !tt.wantPatch {
					t.Fatal("Patch should not be called when watch label is already set")
				}
				patched = true
				if tt.assertPatch != nil {
					tt.assertPatch(t, obj)
				}
				return nil
			})

			r := &Reconciler{ctx: context.Background(), CtrlClient: cached, UncachedClient: uncached}
			if err := r.updateWatchLabel(key, &corev1.ConfigMap{}); err != nil {
				t.Fatalf("updateWatchLabel() error = %v", err)
			}
			if tt.wantPatch && !patched {
				t.Fatal("expected Patch to be called")
			}
		})
	}
}

func TestLabelMatchPredicateUpdate(t *testing.T) {
	t.Parallel()

	watchLabels := map[string]string{WatchedResourceLabelKey: WatchedResourceLabelValue}
	managedLabels := map[string]string{ManagedResourceLabelKey: ManagedResourceLabelValue}

	tests := []struct {
		name      string
		oldLabels map[string]string
		newLabels map[string]string
		want      bool
	}{
		{
			name:      "both old and new watched",
			oldLabels: watchLabels,
			newLabels: watchLabels,
			want:      true,
		},
		{
			name:      "old watched only",
			oldLabels: watchLabels,
			newLabels: map[string]string{"foo": "bar"},
			want:      true,
		},
		{
			name:      "new watched only",
			oldLabels: nil,
			newLabels: watchLabels,
			want:      true,
		},
		{
			name:      "neither watched",
			oldLabels: map[string]string{"foo": "bar"},
			newLabels: map[string]string{"foo": "bar"},
			want:      false,
		},
		{
			name:      "managed label on old only",
			oldLabels: managedLabels,
			newLabels: map[string]string{"foo": "bar"},
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			updateEvent := event.UpdateEvent{
				ObjectOld: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: tt.oldLabels}},
				ObjectNew: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Labels: tt.newLabels}},
			}

			gotManaged := labelMatchPredicate(isManagedResource).Update(updateEvent)
			wantManaged := isManagedResource(updateEvent.ObjectOld) || isManagedResource(updateEvent.ObjectNew)
			if gotManaged != wantManaged {
				t.Fatalf("managed Update() = %v, want %v", gotManaged, wantManaged)
			}

			gotManagedOrWatched := labelMatchPredicate(isManagedOrWatchedResource).Update(updateEvent)
			if gotManagedOrWatched != tt.want {
				t.Fatalf("managedOrWatched Update() = %v, want %v", gotManagedOrWatched, tt.want)
			}
		})
	}
}
