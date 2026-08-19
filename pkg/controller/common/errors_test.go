package common

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestIsUserConfigurationNotFound(t *testing.T) {
	t.Parallel()

	cmGR := schema.GroupResource{Resource: "configmaps"}
	notFound := apierrors.NewNotFound(cmGR, "missing")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "user configuration error wrapping NotFound",
			err:  NewUserConfigurationError(notFound, "trustedCABundle ConfigMap %q not found", "external-secrets/missing"),
			want: true,
		},
		{
			name: "wrapped user configuration NotFound",
			err:  fmt.Errorf("reconcile deployment: %w", NewUserConfigurationError(notFound, "trustedCABundle ConfigMap %q not found", "external-secrets/missing")),
			want: true,
		},
		{
			name: "user configuration error missing key",
			err: NewUserConfigurationError(
				fmt.Errorf("key %q not found", "ca.crt"),
				"trustedCABundle ConfigMap %q does not contain key %q",
				"external-secrets/cm", "ca.crt",
			),
			want: false,
		},
		{
			name: "user configuration error invalid PEM",
			err:  NewUserConfigurationError(errors.New("trusted CA bundle contains no valid PEM-encoded CA certificates"), "trustedCABundle ConfigMap %q key %q has invalid PEM", "external-secrets/cm", "ca.crt"),
			want: false,
		},
		{
			name: "user configuration error invalid proxy URL",
			err:  NewUserConfigurationError(errors.New("invalid proxy URL configured"), "proxy configuration validation failed"),
			want: false,
		},
		{
			name: "bare NotFound",
			err:  notFound,
			want: false,
		},
		{
			name: "retry required reconcile error",
			err:  NewRetryRequiredError(notFound, "fetch configmap %q", "user-ca"),
			want: false,
		},
		{
			name: "irrecoverable reconcile error",
			err:  NewIrrecoverableError(apierrors.NewForbidden(cmGR, "user-ca", errors.New("denied")), "forbidden"),
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("something failed"),
			want: false,
		},
		{name: "nil error", err: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsUserConfigurationNotFound(tt.err); got != tt.want {
				t.Fatalf("IsUserConfigurationNotFound() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsUserConfigurationError(t *testing.T) {
	t.Parallel()

	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "missing")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "trusted CA bundle user configuration error",
			err:  NewUserConfigurationError(notFound, "trustedCABundle ConfigMap %q not found", "external-secrets/missing"),
			want: true,
		},
		{
			name: "proxy user configuration error",
			err:  NewUserConfigurationError(errors.New("invalid proxy URL"), "proxy configuration validation failed"),
			want: true,
		},
		{
			name: "wrapped user configuration error",
			err:  fmt.Errorf("apply user CA: %w", NewUserConfigurationError(errors.New("invalid pem"), "invalid PEM")),
			want: true,
		},
		{
			name: "client NotFound maps to retry required",
			err:  FromClientError(notFound, "failed to fetch trustedCABundle ConfigMap %q", "external-secrets/missing"),
			want: false,
		},
		{
			name: "irrecoverable reconcile error",
			err:  NewIrrecoverableError(apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "user-ca", errors.New("denied")), "forbidden"),
			want: false,
		},
		{
			name: "retry required reconcile error",
			err:  NewRetryRequiredError(errors.New("timeout"), "temporary failure"),
			want: false,
		},
		{name: "bare NotFound", err: notFound, want: false},
		{name: "nil error", err: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsUserConfigurationError(tt.err); got != tt.want {
				t.Fatalf("IsUserConfigurationError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsIrrecoverableError(t *testing.T) {
	t.Parallel()

	cmGR := schema.GroupResource{Resource: "configmaps"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "irrecoverable reconcile error",
			err:  NewIrrecoverableError(errors.New("bad config"), "invalid configuration"),
			want: true,
		},
		{
			name: "forbidden client error",
			err:  FromClientError(apierrors.NewForbidden(cmGR, "user-ca", errors.New("denied")), "patch configmap %q", "user-ca"),
			want: true,
		},
		{
			name: "unauthorized client error",
			err:  FromClientError(apierrors.NewUnauthorized("unauthorized"), "get configmap"),
			want: true,
		},
		{
			name: "invalid client error",
			err:  FromClientError(apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "user-ca", nil), "invalid configmap"),
			want: true,
		},
		{
			name: "NotFound client error is retry required",
			err:  FromClientError(apierrors.NewNotFound(cmGR, "user-ca"), "get configmap %q", "user-ca"),
			want: false,
		},
		{
			name: "user configuration error",
			err:  NewUserConfigurationError(apierrors.NewNotFound(cmGR, "missing"), "trustedCABundle ConfigMap %q not found", "external-secrets/missing"),
			want: false,
		},
		{
			name: "retry required reconcile error",
			err:  NewRetryRequiredError(errors.New("conflict"), "update conflict"),
			want: false,
		},
		{name: "nil error", err: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsIrrecoverableError(tt.err); got != tt.want {
				t.Fatalf("IsIrrecoverableError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFromClientError(t *testing.T) {
	t.Parallel()

	cmGR := schema.GroupResource{Resource: "configmaps"}

	tests := []struct {
		name       string
		err        error
		wantReason ErrorReason
		wantNil    bool
	}{
		{name: "nil error", err: nil, wantNil: true},
		{
			name:       "NotFound",
			err:        apierrors.NewNotFound(cmGR, "user-ca"),
			wantReason: RetryRequiredError,
		},
		{
			name:       "Forbidden",
			err:        apierrors.NewForbidden(cmGR, "user-ca", errors.New("denied")),
			wantReason: IrrecoverableError,
		},
		{
			name:       "Unauthorized",
			err:        apierrors.NewUnauthorized("unauthorized"),
			wantReason: IrrecoverableError,
		},
		{
			name:       "Invalid",
			err:        apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "user-ca", nil),
			wantReason: IrrecoverableError,
		},
		{
			name:       "BadRequest",
			err:        apierrors.NewBadRequest("bad request"),
			wantReason: IrrecoverableError,
		},
		{
			name:       "ServiceUnavailable",
			err:        apierrors.NewServiceUnavailable("service unavailable"),
			wantReason: RetryRequiredError,
		},
		{
			name:       "Conflict",
			err:        apierrors.NewConflict(cmGR, "user-ca", errors.New("conflict")),
			wantReason: RetryRequiredError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FromClientError(tt.err, "operation failed for %q", "user-ca")
			if tt.wantNil {
				if got != nil {
					t.Fatalf("FromClientError() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("FromClientError() = nil, want reconcile error")
			}
			if got.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if !errors.Is(got, tt.err) {
				t.Fatalf("expected wrapped error %v, got %v", tt.err, got.Err)
			}
		})
	}
}

func TestReconcileErrorConstructorsNil(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  *ReconcileError
	}{
		{
			name: "NewIrrecoverableError",
			got:  NewIrrecoverableError(nil, "msg"),
		},
		{
			name: "NewRetryRequiredError",
			got:  NewRetryRequiredError(nil, "msg"),
		},
		{
			name: "NewUserConfigurationError",
			got:  NewUserConfigurationError(nil, "msg"),
		},
		{
			name: "FromClientError",
			got:  FromClientError(nil, "msg"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != nil {
				t.Fatalf("%s(nil) = %v, want nil", tt.name, tt.got)
			}
		})
	}
}
