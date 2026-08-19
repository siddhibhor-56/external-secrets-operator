package external_secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/external-secrets-operator/pkg/controller/client/fakes"
	"github.com/openshift/external-secrets-operator/pkg/controller/commontest"
)

func TestHasManagedOrWatchLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "managed label",
			labels: map[string]string{ManagedResourceLabelKey: ManagedResourceLabelValue},
			want:   true,
		},
		{
			name:   "watch label",
			labels: map[string]string{WatchedResourceLabelKey: WatchedResourceLabelValue},
			want:   true,
		},
		{
			name: "both labels",
			labels: map[string]string{
				ManagedResourceLabelKey: ManagedResourceLabelValue,
				WatchedResourceLabelKey: WatchedResourceLabelValue},
			want: true,
		},
		{
			name:   "unrelated labels",
			labels: map[string]string{"foo": "bar"},
			want:   false,
		},
		{
			name:   "wrong watch value",
			labels: map[string]string{WatchedResourceLabelKey: "false"},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasManagedOrWatchLabel(tt.labels); got != tt.want {
				t.Fatalf("hasManagedOrWatchLabel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildCacheObjectList_ConfigMapNamespaceScope(t *testing.T) {
	t.Parallel()

	objectList := buildCacheObjectList(false)
	var byObject cache.ByObject
	var found bool
	for obj, cfg := range objectList {
		if _, ok := obj.(*corev1.ConfigMap); ok {
			byObject = cfg
			found = true
			break
		}
	}
	if !found {
		t.Fatal("ConfigMap not in cache object list")
	}
	if _, ok := byObject.Namespaces[OperandDefaultNamespace]; !ok {
		t.Fatalf("expected ConfigMap cache scoped to %q, got namespaces %v", OperandDefaultNamespace, byObject.Namespaces)
	}
	if byObject.Label != nil {
		t.Fatalf("expected no label selector on namespace-scoped ConfigMap cache, got %v", byObject.Label)
	}
}

func TestDeploymentFailureConditions(t *testing.T) {
	t.Parallel()

	const observedGeneration int64 = 5

	tests := []struct {
		name                 string
		reconcileErr         error
		wantDegradedStatus   metav1.ConditionStatus
		wantReadyStatus      metav1.ConditionStatus
		wantReadyReason      string
		wantDegradedContains string
		wantReadyContains    string
	}{
		{
			name:                 "irrecoverable error",
			reconcileErr:         common.NewIrrecoverableError(errors.New("bad config"), "invalid configuration"),
			wantDegradedStatus:   metav1.ConditionTrue,
			wantReadyStatus:      metav1.ConditionFalse,
			wantReadyReason:      operatorv1alpha1.ReasonFailed,
			wantDegradedContains: "irrecoverable error",
			wantReadyContains:    "irrecoverable error",
		},
		{
			name: "trusted CA bundle error",
			reconcileErr: common.NewUserConfigurationError(
				errors.New("invalid pem"),
				"trustedCABundle ConfigMap %q key %q has invalid PEM",
				"external-secrets/user-ca", "ca-bundle.crt",
			),
			wantDegradedStatus:   metav1.ConditionTrue,
			wantReadyStatus:      metav1.ConditionFalse,
			wantReadyReason:      operatorv1alpha1.ReasonFailed,
			wantDegradedContains: "user configuration is invalid",
			wantReadyContains:    "user configuration is invalid",
		},
		{
			name: "proxy configuration error",
			reconcileErr: common.NewUserConfigurationError(
				errors.New("invalid proxy URL configured"),
				"externalsecretsconfigs.operator.openshift.io/cluster proxy configuration validation failed",
			),
			wantDegradedStatus:   metav1.ConditionTrue,
			wantReadyStatus:      metav1.ConditionFalse,
			wantReadyReason:      operatorv1alpha1.ReasonFailed,
			wantDegradedContains: "user configuration is invalid",
			wantReadyContains:    "user configuration is invalid",
		},
		{
			name:               "retry required error",
			reconcileErr:       common.NewRetryRequiredError(errors.New("conflict"), "failed to update deployment"),
			wantDegradedStatus: metav1.ConditionFalse,
			wantReadyStatus:    metav1.ConditionFalse,
			wantReadyReason:    operatorv1alpha1.ReasonInProgress,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			degraded, ready := deploymentFailureConditions(observedGeneration, tt.reconcileErr)

			if degraded.ObservedGeneration != observedGeneration {
				t.Fatalf("degraded ObservedGeneration = %d, want %d", degraded.ObservedGeneration, observedGeneration)
			}
			if ready.ObservedGeneration != observedGeneration {
				t.Fatalf("ready ObservedGeneration = %d, want %d", ready.ObservedGeneration, observedGeneration)
			}
			if degraded.Status != tt.wantDegradedStatus {
				t.Fatalf("degraded status = %q, want %q", degraded.Status, tt.wantDegradedStatus)
			}
			if ready.Status != tt.wantReadyStatus {
				t.Fatalf("ready status = %q, want %q", ready.Status, tt.wantReadyStatus)
			}
			if tt.wantReadyReason != "" && ready.Reason != tt.wantReadyReason {
				t.Fatalf("ready reason = %q, want %q", ready.Reason, tt.wantReadyReason)
			}
			if tt.wantDegradedContains != "" && !strings.Contains(degraded.Message, tt.wantDegradedContains) {
				t.Fatalf("degraded message = %q, want substring %q", degraded.Message, tt.wantDegradedContains)
			}
			if tt.wantReadyContains != "" && !strings.Contains(ready.Message, tt.wantReadyContains) {
				t.Fatalf("ready message = %q, want substring %q", ready.Message, tt.wantReadyContains)
			}
		})
	}
}

func TestUserConfigurationFailureResult(t *testing.T) {
	t.Parallel()

	cmGR := schema.GroupResource{Resource: "configmaps"}
	notFoundErr := common.NewUserConfigurationError(
		apierrors.NewNotFound(cmGR, "user-ca"),
		"trustedCABundle ConfigMap %q not found",
		"external-secrets/user-ca",
	)
	invalidErr := common.NewUserConfigurationError(
		errors.New("invalid pem"),
		"trustedCABundle ConfigMap %q key %q has invalid PEM",
		"external-secrets/user-ca", "ca-bundle.crt",
	)
	proxyErr := common.NewUserConfigurationError(
		errors.New("invalid proxy URL configured"),
		"proxy configuration validation failed",
	)

	tests := []struct {
		name            string
		reconcileErr    error
		errUpdate       error
		wantRequeue     time.Duration
		wantReturnError error
	}{
		{
			name:         "NotFound requeues",
			reconcileErr: notFoundErr,
			wantRequeue:  common.DefaultRequeueTime,
		},
		{
			name:         "invalid PEM waits for ConfigMap watch",
			reconcileErr: invalidErr,
		},
		{
			name:         "invalid proxy waits for CR update",
			reconcileErr: proxyErr,
		},
		{
			name:            "status update error is returned",
			reconcileErr:    invalidErr,
			errUpdate:       errors.New("status update failed"),
			wantReturnError: errors.New("status update failed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := userConfigurationFailureResult(tt.reconcileErr, tt.errUpdate)
			if tt.wantReturnError != nil {
				if err == nil || err.Error() != tt.wantReturnError.Error() {
					t.Fatalf("userConfigurationFailureResult() err = %v, want %v", err, tt.wantReturnError)
				}
				return
			}
			if err != nil {
				t.Fatalf("userConfigurationFailureResult() unexpected err = %v", err)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, tt.wantRequeue)
			}
		})
	}
}

func TestReconcileDeploymentFailureResult(t *testing.T) {
	t.Parallel()

	cmGR := schema.GroupResource{Resource: "configmaps"}
	notFoundErr := fmt.Errorf("failed to apply user CA bundle config: %w",
		common.NewUserConfigurationError(
			apierrors.NewNotFound(cmGR, "user-ca"),
			"trustedCABundle ConfigMap %q not found",
			"external-secrets/user-ca",
		),
	)
	irrecoverableErr := common.NewIrrecoverableError(errors.New("forbidden"), "permission denied")
	statusUpdateErr := errors.New("status update failed")
	retryErr := common.NewRetryRequiredError(errors.New("timeout"), "temporary failure")
	proxyErr := common.NewUserConfigurationError(errors.New("invalid proxy URL configured"), "proxy configuration validation failed")

	tests := []struct {
		name            string
		reconcileErr    error
		statusUpdateErr error
		wantRequeue     time.Duration
		wantReturnError error
	}{
		{
			name:         "trusted CA NotFound requeues",
			reconcileErr: notFoundErr,
			wantRequeue:  common.DefaultRequeueTime,
		},
		{
			name:         "irrecoverable error does not requeue",
			reconcileErr: irrecoverableErr,
		},
		{
			name:            "irrecoverable error returns status update failure",
			reconcileErr:    irrecoverableErr,
			statusUpdateErr: statusUpdateErr,
			wantReturnError: statusUpdateErr,
		},
		{
			name:         "proxy configuration error waits for CR update",
			reconcileErr: proxyErr,
		},
		{
			name:         "retry required error requeues",
			reconcileErr: retryErr,
			wantRequeue:  common.DefaultRequeueTime,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := testReconciler(t)
			esc := commontest.TestExternalSecretsConfig()
			mock := &fakes.FakeCtrlClient{}
			mock.GetCalls(func(_ context.Context, _ types.NamespacedName, obj client.Object) error {
				esc.DeepCopyInto(obj.(*operatorv1alpha1.ExternalSecretsConfig))
				return nil
			})
			mock.StatusUpdateCalls(func(_ context.Context, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return tt.statusUpdateErr
			})
			r.CtrlClient = mock

			result, err := r.reconcileDeploymentFailureResult(
				esc,
				types.NamespacedName{Name: common.ExternalSecretsConfigObjectName},
				tt.reconcileErr,
				1,
			)
			if tt.wantReturnError != nil {
				if err == nil {
					t.Fatalf("reconcileDeploymentFailureResult() err = nil, want %v", tt.wantReturnError)
				}
				if err.Error() != tt.wantReturnError.Error() && !strings.Contains(err.Error(), tt.wantReturnError.Error()) {
					t.Fatalf("reconcileDeploymentFailureResult() err = %v, want %v", err, tt.wantReturnError)
				}
			} else if err != nil {
				t.Fatalf("reconcileDeploymentFailureResult() unexpected err = %v", err)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, tt.wantRequeue)
			}
		})
	}
}
