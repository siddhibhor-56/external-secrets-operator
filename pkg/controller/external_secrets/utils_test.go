package external_secrets

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/openshift/external-secrets-operator/pkg/controller/client/fakes"
	"github.com/openshift/external-secrets-operator/pkg/controller/commontest"
)

func TestCreateWithFallback(t *testing.T) {
	tests := []struct {
		name            string
		setupCached     func(*fakes.FakeCtrlClient)
		setupUncached   func(*fakes.FakeCtrlClient)
		wantErr         bool
		wantErrContains string
	}{
		{
			name: "successful create",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return nil
				})
			},
		},
		{
			name: "non-AlreadyExists error propagates",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr:         true,
			wantErrContains: "failed to create",
		},
		{
			name: "AlreadyExists triggers uncached update",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "", Resource: "services"}, "test-svc")
				})
			},
			setupUncached: func(m *fakes.FakeCtrlClient) {
				m.UpdateWithRetryCalls(func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
					return nil
				})
			},
		},
		{
			name: "AlreadyExists with uncached update failure",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "", Resource: "services"}, "test-svc")
				})
			},
			setupUncached: func(m *fakes.FakeCtrlClient) {
				m.UpdateWithRetryCalls(func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr:         true,
			wantErrContains: "failed to restore",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			cachedMock := &fakes.FakeCtrlClient{}
			uncachedMock := &fakes.FakeCtrlClient{}

			if tt.setupCached != nil {
				tt.setupCached(cachedMock)
			}
			if tt.setupUncached != nil {
				tt.setupUncached(uncachedMock)
			}

			r.CtrlClient = cachedMock
			r.UncachedClient = uncachedMock

			esc := commontest.TestExternalSecretsConfig()
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-svc",
					Namespace: "external-secrets",
					Labels:    controllerDefaultResourceLabels,
				},
			}

			err := r.createWithFallback(esc, svc, "Service external-secrets/test-svc")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrContains)
				}
				if tt.wantErrContains != "" && !containsString(err.Error(), tt.wantErrContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCreateWithMetadataFallback(t *testing.T) {
	tests := []struct {
		name            string
		setupCached     func(*fakes.FakeCtrlClient)
		setupUncached   func(*fakes.FakeCtrlClient)
		wantErr         bool
		wantErrContains string
	}{
		{
			name: "successful create",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return nil
				})
			},
		},
		{
			name: "non-AlreadyExists error propagates",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr:         true,
			wantErrContains: "failed to create",
		},
		{
			name: "AlreadyExists triggers metadata patch",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "", Resource: "secrets"}, "test-secret")
				})
			},
			setupUncached: func(m *fakes.FakeCtrlClient) {
				m.PatchCalls(func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					return nil
				})
			},
		},
		{
			name: "AlreadyExists with patch failure",
			setupCached: func(m *fakes.FakeCtrlClient) {
				m.CreateCalls(func(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "", Resource: "secrets"}, "test-secret")
				})
			},
			setupUncached: func(m *fakes.FakeCtrlClient) {
				m.PatchCalls(func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					return commontest.ErrTestClient
				})
			},
			wantErr:         true,
			wantErrContains: "failed to patch metadata",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReconciler(t)
			cachedMock := &fakes.FakeCtrlClient{}
			uncachedMock := &fakes.FakeCtrlClient{}

			if tt.setupCached != nil {
				tt.setupCached(cachedMock)
			}
			if tt.setupUncached != nil {
				tt.setupUncached(uncachedMock)
			}

			r.CtrlClient = cachedMock
			r.UncachedClient = uncachedMock

			esc := commontest.TestExternalSecretsConfig()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "external-secrets",
					Labels:    controllerDefaultResourceLabels,
				},
			}

			err := r.createWithMetadataFallback(esc, secret, "secret resource external-secrets/test-secret")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrContains)
				}
				if tt.wantErrContains != "" && !containsString(err.Error(), tt.wantErrContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLabelMatchPredicate(t *testing.T) {
	pred := labelMatchPredicate()

	managedLabels := map[string]string{
		requestEnqueueLabelKey: requestEnqueueLabelValue,
	}
	unmanagedLabels := map[string]string{
		"app": "something-else",
	}

	managedObj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Labels: managedLabels},
	}
	unmanagedObj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Labels: unmanagedLabels},
	}
	noLabelObj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{},
	}

	tests := []struct {
		name string
		test func() bool
		want bool
	}{
		{
			name: "Create with managed label matches",
			test: func() bool {
				return pred.Create(event.CreateEvent{Object: managedObj})
			},
			want: true,
		},
		{
			name: "Create without managed label does not match",
			test: func() bool {
				return pred.Create(event.CreateEvent{Object: unmanagedObj})
			},
			want: false,
		},
		{
			name: "Update both managed matches",
			test: func() bool {
				return pred.Update(event.UpdateEvent{ObjectOld: managedObj, ObjectNew: managedObj})
			},
			want: true,
		},
		{
			name: "Update old managed new unmanaged matches (label removal)",
			test: func() bool {
				return pred.Update(event.UpdateEvent{ObjectOld: managedObj, ObjectNew: unmanagedObj})
			},
			want: true,
		},
		{
			name: "Update old unmanaged new managed matches (label added)",
			test: func() bool {
				return pred.Update(event.UpdateEvent{ObjectOld: unmanagedObj, ObjectNew: managedObj})
			},
			want: true,
		},
		{
			name: "Update both unmanaged does not match",
			test: func() bool {
				return pred.Update(event.UpdateEvent{ObjectOld: unmanagedObj, ObjectNew: unmanagedObj})
			},
			want: false,
		},
		{
			name: "Delete with managed label matches",
			test: func() bool {
				return pred.Delete(event.DeleteEvent{Object: managedObj})
			},
			want: true,
		},
		{
			name: "Delete without managed label does not match",
			test: func() bool {
				return pred.Delete(event.DeleteEvent{Object: noLabelObj})
			},
			want: false,
		},
		{
			name: "Generic with managed label matches",
			test: func() bool {
				return pred.Generic(event.GenericEvent{Object: managedObj})
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.test()
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
