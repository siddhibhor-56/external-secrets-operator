# API Contracts Guidelines

Guidelines for defining and validating CRD types in the external-secrets-operator. All rules are drawn from existing conventions in `api/v1alpha1/`.

## General Rules

- **No functions in the API package.** `api/v1alpha1/` must contain only type definitions, constants, kubebuilder markers, and generated code (deepcopy). Business logic, helpers, and utility functions belong in `pkg/controller/` or `pkg/operator/`. Exception: standard kubebuilder scaffolding (`init()` / `SchemeBuilder.Register()`, and `Resource()` in `groupversion_info.go`) is allowed.
- **Godoc is user-facing.** Field comments are extracted into API reference docs (`make docs`). Write them for cluster administrators, not only for operator developers. See [Godoc Requirements](#13-godoc-requirements).

## 1. Singleton Enforcement

Both CRDs are cluster-scoped singletons. Enforce with a CEL rule on the top-level type:

```go
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="<Kind> is a singleton, .metadata.name must be 'cluster'"
```

Test both the happy path (`resourceName: cluster`) and a rejection case in the `.testsuite.yaml`.

## 2. Field Immutability

Use `self == oldSelf` on the field itself, not on the parent struct:

```go
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="mode is immutable once set"
```

When applied to an object field, the entire sub-tree becomes immutable. Always add a corresponding `onUpdate` test that asserts a stable error substring (see [Test Patterns](#14-test-patterns-testsuiteyaml)).

## 3. List Map Keys and `listType=map`

Ordered lists that must be merge-patched by a unique key use:

```go
// +listType=map
// +listMapKey=<fieldName>
```

Composite keys are supported by repeating `+listMapKey`. Write a test that submits duplicates and asserts the `Duplicate value` error. When a list is not merge-patched (e.g., tolerations), use `+listType=atomic`.

## 4. `nolint:kubeapilinter` for listMapKey Fields

Fields that serve as `listMapKey` must NOT carry `omitempty` in their JSON tag. Suppress the kube-api-linter warning with a reason:

```go
//nolint:kubeapilinter // Name is a listMapKey and must not have omitempty for proper patch identification
Name string `json:"name"`
```

## 5. CEL Cross-Field Validation

Place rules on the lowest struct that contains all referenced fields. Guard every path segment with `has()`:

```go
// +kubebuilder:validation:XValidation:rule="self.mode != 'Enabled' || has(self.issuerRef)",message="issuerRef must be provided when mode is set to Enabled."
```

Test both failing and passing paths.

## 6. List Key Immutability via CEL

To make list map keys immutable while allowing other field changes:

```go
// +kubebuilder:validation:XValidation:rule="oldSelf.all(op, self.exists(p, p.name == op.name && p.componentName == op.componentName))",message="name and componentName fields in networkPolicies are immutable"
```

New entries are allowed; removal or key changes are rejected.

## 7. Reserved Domain Blocking

Annotations use layered CEL rules: key format regex, prefix length <= 253, name part <= 63, reserved domain block. Each reserved domain group gets its own rule and message. Test subdomains, non-matching lookalikes, and max-length boundaries.

## 8. Reserved Environment Variable Names

Block reserved names/prefixes with a single CEL expression on the list:

```go
// +kubebuilder:validation:XValidation:rule="self.all(e, !['KUBERNETES_', 'EXTERNAL_SECRETS_'].exists(p, e.name.startsWith(p)) && e.name != 'HOSTNAME' && e.name != 'SSL_CERT_DIR' && e.name != 'SSL_CERT_FILE')"
```

Test that exact matches are blocked but superstrings are allowed (e.g., `HOSTNAME_SUFFIX` is valid).

## 9. Enum Validation

Mode/state fields use explicit enum markers:

```go
// +kubebuilder:validation:Enum:=Enabled;Disabled
```

Note: The codebase has one case using `Enum=` instead of `Enum:=` for `ManagementState`. Define a named Go type with `const` values. Use case-insensitive CEL (`lowerAscii()`) only when the API must accept mixed-case input.

## 10. Bounds

| Constraint | Typical values |
|---|---|
| Kubernetes names | MinLength=1, MaxLength=253 |
| Namespace names | MinLength=1, MaxLength=63 |
| Proxy URLs | MinLength=0, MaxLength=2048 |
| Labels/annotations maps | MinProperties=0, MaxProperties=20 |
| Tolerations | MinItems=0, MaxItems=50 |
| NetworkPolicies | MinItems=0, MaxItems=50 |
| ComponentConfigs | MinItems=0, MaxItems=4 |
| logLevel | Min=1, Max=5 |
| revisionHistoryLimit | Min=1, Max=50 |

## 11. Defaults

**Going forward:** prefer defaulting within the controller, not in the CRD schema. Per OpenShift API conventions: *"With configuration APIs, we typically default fields within the controller and not within the API. This means that the platform has the ability to make changes to the defaults over time."*

New fields should omit `+kubebuilder:default` and apply defaults at reconcile time so defaults can evolve across releases without CRD schema migrations.

Existing fields that already use `+kubebuilder:default` are legacy — do not add more:

| Field | Default |
|---|---|
| `logLevel` | `1` |
| `mode` | `Disabled` |
| `injectAnnotations` | `"false"` |
| `certificateCheckInterval` | `"5m"` |
| `certificateDuration` | `"8760h"` |
| `certificateRenewBefore` | `"30m"` |
| `revisionHistoryLimit` | `10` |
| `networkPolicyProvisioning` | `Managed` |
| `key` (`ConfigMapKeyReference`) | `"ca-bundle.crt"` |

## 12. Kubebuilder Markers Reference

| Marker | Placement |
|---|---|
| `+kubebuilder:object:root=true` | Top-level CRD type and List type |
| `+kubebuilder:subresource:status` | Top-level CRD type |
| `+kubebuilder:resource:path=...,scope=Cluster` | Top-level CRD type |
| `+kubebuilder:validation:XValidation` | Type or field for CEL rules |
| `+kubebuilder:validation:Enum` | Fields with closed set of values |
| `+listType=map/atomic` | Slice fields |
| `+listMapKey=<key>` | Slice fields with `listType=map` |
| `+mapType=granular/atomic` | Map fields |
| `+optional` / `+required` | Every exported field |

## 13. Godoc Requirements

Every exported field in `api/v1alpha1/` types must have a Godoc comment that:

- Explains the field's **purpose** clearly enough for an end user unfamiliar with the implementation
- Documents **interactions** with other fields (e.g., "Only relevant when `certManager.mode` is `Enabled`")
- States **limitations** or constraints (e.g., max length, immutability, allowed values)
- Describes **default behavior** when the field is omitted or zero-valued

Godoc is the primary user-facing API documentation and is extracted into generated API reference docs via `make docs`. Write for a cluster administrator audience.

## 14. Test Patterns (`.testsuite.yaml`)

Tests live in `api/<version>/tests/<kind-lowercase>.<group>/` (singular kind, e.g. `externalsecretsconfig.operator.openshift.io/`) and use the declarative YAML format consumed by `test/apis/generator.go`. Inside the suite YAML, `crdName` is the **plural** Kubernetes CRD name (e.g. `externalsecretsconfigs.operator.openshift.io`).

```yaml
name: "ExternalSecretsConfig"
# Directory: api/v1alpha1/tests/externalsecretsconfig.operator.openshift.io/
# crdName: plural CRD name (not the directory segment)
crdName: externalsecretsconfigs.operator.openshift.io
tests:
  onCreate:
    - name: "Should be able to create a minimal instance"
      resourceName: cluster
      initial: |
        <yaml resource>
      expected: |
        <yaml with defaults applied>
      expectedError: ""
  onUpdate:
    - name: "Should reject immutable field change"
      ...
      expectedError: "field is immutable"
```

Coverage requirements per validation rule:
1. Happy-path create with defaults in `expected`
2. Rejection case with exact error substring in `expectedError`
3. Boundary tests (max length, max items)
4. For immutability: `onUpdate` test changing the immutable field
5. For cross-field rules: all passing and failing combinations
6. For list map keys: duplicate entries, valid multi-entry lists, key immutability

Test names start with "Should" and describe the outcome.
