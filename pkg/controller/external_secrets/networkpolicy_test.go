package external_secrets

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/client/fakes"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	"github.com/openshift/external-secrets-operator/pkg/controller/commontest"
)

// staticNetworkPolicies returns a map of all static network policy names to their asset paths.
func staticNetworkPolicies() map[string]string {
	return map[string]string{
		"eso-sys-deny-all-traffic":                             denyAllNetworkPolicyAssetName,
		"eso-sys-allow-api-server-egress-for-main-controller":  allowMainControllerTrafficAssetName,
		"eso-sys-allow-api-server-egress-for-webhook":          allowWebhookTrafficAssetName,
		"eso-sys-allow-api-server-egress-for-cert-controller":  allowCertControllerTrafficAssetName,
		"eso-sys-allow-api-server-egress-for-bitwarden-server": allowBitwardenServerTrafficAssetName,
		"eso-sys-allow-to-dns":                                 allowDnsTrafficAssetName,
	}
}

func TestCreateOrApplyStaticNetworkPolicies(t *testing.T) {
	tests := []struct {
		name                        string
		preReq                      func(*Reconciler, *fakes.FakeCtrlClient)
		updateExternalSecretsConfig func(*operatorv1alpha1.ExternalSecretsConfig)
		wantErr                     string
	}{
		{
			name: "all static network policies created successfully",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})

				expectedNPMap := make(map[string]*networkingv1.NetworkPolicy)
				for name, path := range staticNetworkPolicies() {
					if name == "eso-sys-allow-api-server-egress-for-cert-controller" ||
						name == "eso-sys-allow-api-server-egress-for-bitwarden-server" {
						continue
					}
					expectedNPMap[name] = testNetworkPolicy(path)
				}

				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
						if _, found := expectedNPMap[np.Name]; found {
							return nil
						}
					}
					return nil
				})
			},
		},
		{
			name: "bitwarden network policy created when enabled",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					expectedNP := testNetworkPolicy(allowBitwardenServerTrafficAssetName)
					if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
						if np.Name == expectedNP.Name {
							return nil
						}
					}
					return nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec = operatorv1alpha1.ExternalSecretsConfigSpec{
					Plugins: operatorv1alpha1.PluginsConfig{
						BitwardenSecretManagerProvider: &operatorv1alpha1.BitwardenSecretManagerProvider{
							Mode: operatorv1alpha1.Enabled,
						},
					},
				}
			},
		},
		{
			name: "cert-controller network policy skipped when cert-manager enabled",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
						if np.Name == "eso-sys-allow-api-server-egress-for-cert-controller" {
							return fmt.Errorf("cert-controller policy should not be created")
						}
					}
					return nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec = operatorv1alpha1.ExternalSecretsConfigSpec{
					ControllerConfig: operatorv1alpha1.ControllerConfig{
						CertProvider: &operatorv1alpha1.CertProvidersConfig{
							CertManager: &operatorv1alpha1.CertManagerConfig{
								Mode: operatorv1alpha1.Enabled,
							},
						},
					},
				}
			},
		},
		{
			name: "network policy exists and needs update",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if o, ok := obj.(*networkingv1.NetworkPolicy); ok {
						np := testNetworkPolicy(denyAllNetworkPolicyAssetName)
						np.Labels = nil
						np.DeepCopyInto(o)
					}
					return true, nil
				})
				m.UpdateWithRetryCalls(func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
					return nil
				})
			},
		},
		{
			name: "network policy creation fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if np, ok := obj.(*networkingv1.NetworkPolicy); ok && np.Name == "eso-sys-deny-all-traffic" {
						return commontest.ErrTestClient
					}
					return nil
				})
			},
			wantErr: "failed to create network policy external-secrets/eso-sys-deny-all-traffic: test client error",
		},
		{
			name: "network policy exists check fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
						return false, commontest.ErrTestClient
					}
					return true, nil
				})
			},
			wantErr: "failed to check existence of network policy external-secrets/eso-sys-deny-all-traffic: test client error",
		},
		{
			name: "network policy update fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if o, ok := obj.(*networkingv1.NetworkPolicy); ok {
						np := testNetworkPolicy(denyAllNetworkPolicyAssetName)
						np.Labels = nil
						np.DeepCopyInto(o)
					}
					return true, nil
				})
				m.UpdateWithRetryCalls(func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
						return commontest.ErrTestClient
					}
					return nil
				})
			},
			wantErr: "failed to update network policy external-secrets/eso-sys-deny-all-traffic: test client error",
		},
		{
			name: "network policy with custom annotations applied successfully",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
						if np.Annotations == nil {
							t.Error("networkpolicy annotations should not be nil")
							return nil
						}
						if np.Annotations["security/policy-type"] != "deny-all" {
							t.Errorf("expected annotation 'security/policy-type'='deny-all', got '%s'",
								np.Annotations["security/policy-type"])
						}
					}
					return nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.Annotations = map[string]string{
					"security/policy-type": "deny-all",
					"team/owner":           "security",
				}
			},
		},
		{
			name: "network policy tracks managed annotations",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
						if np.Annotations["custom-policy"] != "value" {
							t.Errorf("expected 'custom-policy' annotation")
						}
					}
					return nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.Annotations = map[string]string{
					"custom-policy": "value",
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			mock := &fakes.FakeCtrlClient{}
			r.CtrlClient = mock
			if tt.preReq != nil {
				tt.preReq(r, mock)
			}

			esc := commontest.TestExternalSecretsConfig()
			if tt.updateExternalSecretsConfig != nil {
				tt.updateExternalSecretsConfig(esc)
			}

			err := r.createOrApplyStaticNetworkPolicies(esc, testResourceMetadata(esc), false)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("Expected error: %v, got: %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestCreateOrApplyCustomNetworkPolicies(t *testing.T) {
	tests := []struct {
		name                        string
		preReq                      func(*Reconciler, *fakes.FakeCtrlClient)
		updateExternalSecretsConfig func(*operatorv1alpha1.ExternalSecretsConfig)
		wantErr                     string
	}{
		{
			name: "no custom network policies configured",
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.NetworkPolicies = nil
			},
		},
		{
			name: "custom network policy created with eso-user prefix",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if np, ok := obj.(*networkingv1.NetworkPolicy); ok {
						expected := userNetworkPolicyPrefix + "test-custom-policy"
						if np.Name != expected {
							return fmt.Errorf("expected network policy name %q, got %q", expected, np.Name)
						}
					}
					return nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.NetworkPolicies = []operatorv1alpha1.NetworkPolicy{
					{
						Name:          "test-custom-policy",
						ComponentName: operatorv1alpha1.CoreController,
						Egress: []networkingv1.NetworkPolicyEgressRule{
							{
								Ports: []networkingv1.NetworkPolicyPort{
									{
										Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
										Port:     &[]intstr.IntOrString{intstr.FromInt(443)}[0],
									},
								},
							},
						},
					},
				}
			},
		},
		{
			name: "custom network policy with invalid component name",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.NetworkPolicies = []operatorv1alpha1.NetworkPolicy{
					{
						Name:          "test-invalid-policy",
						ComponentName: "InvalidComponent",
						Egress:        []networkingv1.NetworkPolicyEgressRule{},
					},
				}
			},
			wantErr: "failed to determine pod selector for network policy test-invalid-policy: unknown component name: InvalidComponent",
		},
		{
			name: "custom network policy creation fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
						return commontest.ErrTestClient
					}
					return nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.NetworkPolicies = []operatorv1alpha1.NetworkPolicy{
					{
						Name:          "test-fail-policy",
						ComponentName: operatorv1alpha1.CoreController,
						Egress:        []networkingv1.NetworkPolicyEgressRule{},
					},
				}
			},
			wantErr: "failed to create network policy external-secrets/eso-user-test-fail-policy: test client error",
		},
		{
			name: "custom network policy updated successfully",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if o, ok := obj.(*networkingv1.NetworkPolicy); ok {
						np := &networkingv1.NetworkPolicy{
							ObjectMeta: metav1.ObjectMeta{
								Name:      userNetworkPolicyPrefix + "test-update-policy",
								Namespace: externalsecretsDefaultNamespace,
							},
						}
						np.DeepCopyInto(o)
					}
					return true, nil
				})
				m.UpdateWithRetryCalls(func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
					return nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ControllerConfig.NetworkPolicies = []operatorv1alpha1.NetworkPolicy{
					{
						Name:          "test-update-policy",
						ComponentName: operatorv1alpha1.CoreController,
						Egress: []networkingv1.NetworkPolicyEgressRule{
							{
								Ports: []networkingv1.NetworkPolicyPort{
									{
										Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
										Port:     &[]intstr.IntOrString{intstr.FromInt(443)}[0],
									},
								},
							},
						},
					},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			mock := &fakes.FakeCtrlClient{}
			r.CtrlClient = mock
			if tt.preReq != nil {
				tt.preReq(r, mock)
			}

			esc := commontest.TestExternalSecretsConfig()
			if tt.updateExternalSecretsConfig != nil {
				tt.updateExternalSecretsConfig(esc)
			}

			err := r.createOrApplyCustomNetworkPolicies(esc, testResourceMetadata(esc), false)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("Expected error: %v, got: %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestGetPodSelectorForComponent(t *testing.T) {
	tests := []struct {
		name          string
		componentName operatorv1alpha1.ComponentName
		wantLabels    map[string]string
		wantErr       bool
	}{
		{
			name:          "CoreController component",
			componentName: operatorv1alpha1.CoreController,
			wantLabels: map[string]string{
				"app.kubernetes.io/name": "external-secrets",
			},
			wantErr: false,
		},
		{
			name:          "BitwardenSDKServer component",
			componentName: operatorv1alpha1.BitwardenSDKServer,
			wantLabels: map[string]string{
				"app.kubernetes.io/name": bitwardenSDKServerContainerName,
			},
			wantErr: false,
		},
		{
			name:          "Unknown component",
			componentName: "UnknownComponent",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			selector, err := r.getPodSelectorForComponent(tt.componentName)

			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			if len(selector.MatchLabels) != len(tt.wantLabels) {
				t.Errorf("Expected %d labels, got %d", len(tt.wantLabels), len(selector.MatchLabels))
			}
			for k, v := range tt.wantLabels {
				if selector.MatchLabels[k] != v {
					t.Errorf("Expected label %s=%s, got %s", k, v, selector.MatchLabels[k])
				}
			}
		})
	}
}

func TestBuildNetworkPolicyFromConfig(t *testing.T) {
	tests := []struct {
		name       string
		npConfig   operatorv1alpha1.NetworkPolicy
		wantErr    bool
		wantPolicy func(*networkingv1.NetworkPolicy) bool
	}{
		{
			name: "valid CoreController network policy with eso-user prefix",
			npConfig: operatorv1alpha1.NetworkPolicy{
				Name:          "test-core-policy",
				ComponentName: operatorv1alpha1.CoreController,
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{
							{
								Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
								Port:     &[]intstr.IntOrString{intstr.FromInt(443)}[0],
							},
						},
					},
				},
			},
			wantErr: false,
			wantPolicy: func(np *networkingv1.NetworkPolicy) bool {
				return np.Name == userNetworkPolicyPrefix+"test-core-policy" &&
					np.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] == externalsecretsCommonName &&
					len(np.Spec.Egress) == 1 &&
					len(np.Spec.PolicyTypes) == 1 &&
					np.Spec.PolicyTypes[0] == networkingv1.PolicyTypeEgress
			},
		},
		{
			name: "valid BitwardenSDKServer network policy with eso-user prefix",
			npConfig: operatorv1alpha1.NetworkPolicy{
				Name:          "test-bitwarden-policy",
				ComponentName: operatorv1alpha1.BitwardenSDKServer,
				Egress:        []networkingv1.NetworkPolicyEgressRule{},
			},
			wantErr: false,
			wantPolicy: func(np *networkingv1.NetworkPolicy) bool {
				return np.Name == userNetworkPolicyPrefix+"test-bitwarden-policy" &&
					np.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] == bitwardenSDKServerContainerName
			},
		},
		{
			name: "invalid component name",
			npConfig: operatorv1alpha1.NetworkPolicy{
				Name:          "test-invalid",
				ComponentName: "InvalidComponent",
				Egress:        []networkingv1.NetworkPolicyEgressRule{},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			esc := commontest.TestExternalSecretsConfig()

			np, err := r.buildNetworkPolicyFromConfig(esc, tt.npConfig, testResourceMetadata(esc))

			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if tt.wantPolicy != nil && !tt.wantPolicy(np) {
					t.Errorf("Network policy validation failed: got name=%q", np.Name)
				}
			}
		})
	}
}

func TestExtractProxyPort(t *testing.T) {
	tests := []struct {
		name        string
		proxyConfig *operatorv1alpha1.ProxyConfig
		wantPort    int
		wantErr     bool
	}{
		{
			name: "https proxy with explicit port",
			proxyConfig: &operatorv1alpha1.ProxyConfig{
				HTTPSProxy: "https://proxy.example.com:3129",
			},
			wantPort: 3129,
		},
		{
			name: "http proxy with explicit port",
			proxyConfig: &operatorv1alpha1.ProxyConfig{
				HTTPProxy: "http://proxy.example.com:3128",
			},
			wantPort: 3128,
		},
		{
			name: "https proxy without port defaults to 443",
			proxyConfig: &operatorv1alpha1.ProxyConfig{
				HTTPSProxy: "https://proxy.example.com",
			},
			wantPort: 443,
		},
		{
			name: "http proxy without port defaults to 80",
			proxyConfig: &operatorv1alpha1.ProxyConfig{
				HTTPProxy: "http://proxy.example.com",
			},
			wantPort: 80,
		},
		{
			name: "no proxy URLs defaults to 3128",
			proxyConfig: &operatorv1alpha1.ProxyConfig{
				NoProxy: "localhost",
			},
			wantPort: 3128,
		},
		{
			name: "https proxy takes precedence over http proxy",
			proxyConfig: &operatorv1alpha1.ProxyConfig{
				HTTPSProxy: "https://proxy.example.com:8443",
				HTTPProxy:  "http://proxy.example.com:8080",
			},
			wantPort: 8443,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, err := extractProxyPort(tt.proxyConfig)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			if port != tt.wantPort {
				t.Errorf("Expected port %d, got %d", tt.wantPort, port)
			}
		})
	}
}

func TestReconcileProxyEgressPolicy(t *testing.T) {
	tests := []struct {
		name                        string
		preReq                      func(*Reconciler, *fakes.FakeCtrlClient)
		updateExternalSecretsConfig func(*operatorv1alpha1.ExternalSecretsConfig)
		wantErr                     string
		wantCreated                 bool
		wantDeleted                 bool
	}{
		{
			name: "proxy egress policy created when proxy configured and managed",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					HTTPProxy:                 "http://proxy.example.com:3128",
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateManaged,
				}
			},
			wantCreated: true,
		},
		{
			name: "proxy egress policy not created when unmanaged",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					HTTPProxy:                 "http://proxy.example.com:3128",
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateUnmanaged,
				}
			},
			wantCreated: false,
		},
		{
			name: "proxy egress policy not created when no proxy configured",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, nil
				})
			},
			wantCreated: false,
		},
		{
			name: "proxy egress policy deleted when switching to unmanaged",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					if o, ok := obj.(*networkingv1.NetworkPolicy); ok {
						existing := &networkingv1.NetworkPolicy{
							ObjectMeta: metav1.ObjectMeta{
								Name:      proxyEgressPolicyName,
								Namespace: externalsecretsDefaultNamespace,
							},
						}
						existing.DeepCopyInto(o)
					}
					return true, nil
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					HTTPProxy:                 "http://proxy.example.com:3128",
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateUnmanaged,
				}
			},
			wantDeleted: true,
		},
		{
			name: "proxy egress policy exists check fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ExistsCalls(func(ctx context.Context, ns types.NamespacedName, obj client.Object) (bool, error) {
					return false, commontest.ErrTestClient
				})
			},
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.Spec.ApplicationConfig.Proxy = &operatorv1alpha1.ProxyConfig{
					HTTPProxy:                 "http://proxy.example.com:3128",
					NetworkPolicyProvisioning: operatorv1alpha1.ManagementStateManaged,
				}
			},
			wantErr: "failed to check existence of proxy egress network policy eso-sys-proxy-egress-core: test client error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			mock := &fakes.FakeCtrlClient{}
			r.CtrlClient = mock
			if tt.preReq != nil {
				tt.preReq(r, mock)
			}

			esc := commontest.TestExternalSecretsConfig()
			if tt.updateExternalSecretsConfig != nil {
				tt.updateExternalSecretsConfig(esc)
			}

			err := r.reconcileProxyEgressPolicy(esc, testResourceMetadata(esc), false)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("Expected error: %v, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if tt.wantCreated && mock.CreateCallCount() == 0 {
				t.Error("Expected Create to be called but it was not")
			}
			if !tt.wantCreated && mock.CreateCallCount() > 0 {
				t.Errorf("Expected no Create call but got %d", mock.CreateCallCount())
			}
			if tt.wantDeleted && mock.DeleteCallCount() == 0 {
				t.Error("Expected Delete to be called but it was not")
			}
		})
	}
}

func TestCleanupMigratedNetworkPolicies(t *testing.T) {
	tests := []struct {
		name                        string
		preReq                      func(*Reconciler, *fakes.FakeCtrlClient)
		updateExternalSecretsConfig func(*operatorv1alpha1.ExternalSecretsConfig)
		wantErr                     string
		wantDeleteCount             int
		wantPatchCount              int
	}{
		{
			name: "skip when annotation already set",
			updateExternalSecretsConfig: func(esc *operatorv1alpha1.ExternalSecretsConfig) {
				esc.SetAnnotations(map[string]string{
					skipNPCleanupAnnotation: "true",
				})
			},
			wantDeleteCount: 0,
			wantPatchCount:  0,
		},
		{
			name: "delete stale unprefixed policies",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ListCalls(func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
					if npList, ok := list.(*networkingv1.NetworkPolicyList); ok {
						npList.Items = []networkingv1.NetworkPolicy{
							{ObjectMeta: metav1.ObjectMeta{Name: "eso-sys-deny-all-traffic", Namespace: externalsecretsDefaultNamespace}},
							{ObjectMeta: metav1.ObjectMeta{Name: "deny-all-traffic", Namespace: externalsecretsDefaultNamespace}},
							{ObjectMeta: metav1.ObjectMeta{Name: "allow-to-dns", Namespace: externalsecretsDefaultNamespace}},
						}
					}
					return nil
				})
			},
			wantDeleteCount: 2,
			wantPatchCount:  1,
		},
		{
			name: "no stale policies to delete",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ListCalls(func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
					if npList, ok := list.(*networkingv1.NetworkPolicyList); ok {
						npList.Items = []networkingv1.NetworkPolicy{
							{ObjectMeta: metav1.ObjectMeta{Name: "eso-sys-deny-all-traffic", Namespace: externalsecretsDefaultNamespace}},
							{ObjectMeta: metav1.ObjectMeta{Name: "eso-sys-allow-to-dns", Namespace: externalsecretsDefaultNamespace}},
						}
					}
					return nil
				})
			},
			wantDeleteCount: 0,
			wantPatchCount:  1,
		},
		{
			name: "list fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ListCalls(func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr: "failed to list network policies in external-secrets for cleanup: test client error",
		},
		{
			name: "delete fails",
			preReq: func(r *Reconciler, m *fakes.FakeCtrlClient) {
				m.ListCalls(func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
					if npList, ok := list.(*networkingv1.NetworkPolicyList); ok {
						npList.Items = []networkingv1.NetworkPolicy{
							{ObjectMeta: metav1.ObjectMeta{Name: "stale-policy", Namespace: externalsecretsDefaultNamespace}},
						}
					}
					return nil
				})
				m.DeleteCalls(func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr: "failed to delete stale network policy external-secrets/stale-policy: test client error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			mock := &fakes.FakeCtrlClient{}
			r.CtrlClient = mock
			if tt.preReq != nil {
				tt.preReq(r, mock)
			}

			esc := commontest.TestExternalSecretsConfig()
			if tt.updateExternalSecretsConfig != nil {
				tt.updateExternalSecretsConfig(esc)
			}

			rm := common.ResourceMetadata{
				Labels:                controllerDefaultResourceLabels,
				Annotations:           esc.Spec.ControllerConfig.Annotations,
				DeletedAnnotationKeys: []string{},
			}

			err := r.cleanupMigratedNetworkPolicies(esc, rm)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("Expected error: %v, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if mock.DeleteCallCount() != tt.wantDeleteCount {
				t.Errorf("Expected %d Delete calls, got %d", tt.wantDeleteCount, mock.DeleteCallCount())
			}
			if mock.PatchCallCount() != tt.wantPatchCount {
				t.Errorf("Expected %d Patch calls, got %d", tt.wantPatchCount, mock.PatchCallCount())
			}
		})
	}
}
