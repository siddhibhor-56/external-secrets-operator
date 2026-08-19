package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&ExternalSecretsManager{}, &ExternalSecretsManagerList{})
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// ExternalSecretsManagerList is a list of ExternalSecretsManager objects.
type ExternalSecretsManagerList struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard list's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	metav1.ListMeta `json:"metadata"`

	Items []ExternalSecretsManager `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=externalsecretsmanagers,scope=Cluster,categories={external-secrets-operator, external-secrets},shortName=esm;externalsecretsmanager;esmanager
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:metadata:labels={"app.kubernetes.io/name=externalsecretsmanager", "app.kubernetes.io/part-of=external-secrets-operator"}

// ExternalSecretsManager describes configuration and information about the deployments managed by the external-secrets-operator.
// The name must be `cluster` as this is a singleton object allowing only one instance of ExternalSecretsManager per cluster.
//
// It is mainly for configuring the global options and enabling optional features, which serves as a common/centralized config for managing multiple controllers of the operator.
// The object is automatically created during the operator installation.
//
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="ExternalSecretsManager is a singleton, .metadata.name must be 'cluster'"
// +operator-sdk:csv:customresourcedefinitions:displayName="ExternalSecretsManager"
type ExternalSecretsManager struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +required
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the specification of the desired behavior
	// +optional
	Spec ExternalSecretsManagerSpec `json:"spec,omitempty"`

	// status is the most recently observed status of controllers used by External Secrets Operator.
	// +optional
	Status ExternalSecretsManagerStatus `json:"status,omitempty"`
}

// ExternalSecretsManagerSpec is the specification of the desired behavior of the ExternalSecretsManager.
type ExternalSecretsManagerSpec struct {
	// globalConfig is for configuring the behavior of deployments that are managed by external secrets-operator.
	// +optional
	GlobalConfig *GlobalConfig `json:"globalConfig,omitempty"`

	// features configures optional capabilities across deployments managed by the external-secrets-operator,
	// including the operator itself and any current or future operands.
	// Each entry is uniquely identified by name and can be individually enabled or disabled.
	// This field can have a maximum of 1 entry.
	// +kubebuilder:validation:MinItems:=0
	// +kubebuilder:validation:MaxItems:=1
	// +optional
	// +listType=map
	// +listMapKey=name
	Features []Feature `json:"features,omitempty"`
}

// GlobalConfig is for configuring the external-secrets-operator behavior.
type GlobalConfig struct {
	CommonConfigs `json:",inline"`

	// labels to apply to all resources created by the operator.
	// This field can have a maximum of 20 entries.
	// +mapType=granular
	// +kubebuilder:validation:MinProperties:=0
	// +kubebuilder:validation:MaxProperties:=20
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// Feature configures an optional capability that is applied by the external-secrets-operator across its managed deployments.
type Feature struct {
	// name identifies the optional feature to configure.
	// Currently, the only supported value is UnsafeAllowGenericTargets.
	// +kubebuilder:validation:Enum:=UnsafeAllowGenericTargets
	// +required
	//nolint:kubeapilinter // Name is a listMapKey and must not have omitempty for proper patch identification
	Name FeatureName `json:"name"`

	// mode controls whether the feature is active.
	// When set to Enabled, the operator applies the configuration associated with the named feature to the relevant managed deployments.
	// For UnsafeAllowGenericTargets, this passes the `--unsafe-allow-generic-targets` flag to the external-secrets core controller,
	// allowing ExternalSecret resources to target Kubernetes resources other than Secrets (for example, ConfigMaps or custom resources).
	// Warning: Generic targets require additional RBAC permissions on the affected operand; enabling this feature without the appropriate permissions will cause reconciliation failures.
	// +kubebuilder:validation:Enum:=Enabled;Disabled
	// +kubebuilder:default:=Disabled
	// +optional
	Mode Mode `json:"mode,omitempty"`
}

// ExternalSecretsManagerStatus is the most recently observed status of the ExternalSecretsManager.
type ExternalSecretsManagerStatus struct {
	// controllerStatuses holds the observed conditions of the controllers part of the operator.
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	// +optional
	ControllerStatuses []ControllerStatus `json:"controllerStatuses,omitempty"`

	// lastTransitionTime is the last time the condition transitioned from one status to another.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=date-time
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// ControllerStatus holds the observed conditions of the controllers part of the operator.
type ControllerStatus struct {
	// name of the controller for which the observed condition is recorded.
	// +required
	//nolint:kubeapilinter // Name is a listMapKey and must not have omitempty for proper patch identification
	Name string `json:"name"`

	// conditions holds information of the current state of the external-secrets-operator controllers.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	//nolint:kubeapilinter // custom Condition type is intentional, not all fields of metav1.Condition are needed.
	Conditions []Condition `json:"conditions,omitempty"`

	// observedGeneration represents the .metadata.generation on the observed resource.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

type Condition struct {
	// type of the condition
	// +required
	//nolint:kubeapilinter // Type is a listMapKey and must not have omitempty for proper patch identification
	Type string `json:"type"`

	// status of the condition
	// +optional
	Status metav1.ConditionStatus `json:"status,omitempty"`

	// message provides details about the state.
	// +optional
	Message string `json:"message,omitempty"`
}
