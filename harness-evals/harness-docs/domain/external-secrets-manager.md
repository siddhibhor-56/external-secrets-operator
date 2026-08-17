# ExternalSecretsManager

**API Group**: `operator.openshift.io/v1alpha1`
**Kind**: `ExternalSecretsManager`
**Scope**: Cluster (singleton, name must be `cluster`)
**Short Names**: `esm`, `externalsecretsmanager`, `esmanager`

**API Definition**: [`api/v1alpha1/external_secrets_manager_types.go`](../../api/v1alpha1/external_secrets_manager_types.go)

## Purpose

Global configuration and feature toggle CR. Auto-created by the operator during startup. Provides cluster-wide settings and aggregates status from all operator controllers.

**Key Principle**: ESM is a centralized config that the operator creates and manages. Users modify it for global settings; status is read-only and aggregated from ESC conditions.

## Spec Structure

```go
type ExternalSecretsManagerSpec struct {
    GlobalConfig  *GlobalConfig  // Cluster-wide common configs + labels
    Features      []Feature      // Optional feature toggles (max 1 entry)
}
```

### GlobalConfig

Inherits `CommonConfigs` (logLevel, resources, affinity, tolerations, nodeSelector, proxy) plus:

| Field | Type | Description |
|-------|------|-------------|
| `labels` | `map[string]string` (max 20) | Labels applied to all operator-created resources |

**Label priority** (lowest to highest): ESM GlobalConfig < ESC ControllerConfig < controller default labels.

**Proxy resolution** (highest priority first): ESC spec > ESM GlobalConfig > OLM-injected env vars.

### Features

| Field | Type | Description |
|-------|------|-------------|
| `name` | `FeatureName` (enum) | Feature identifier; currently only `UnsafeAllowGenericTargets` |
| `mode` | `Mode` (Enabled/Disabled) | Feature state, default Disabled |

**UnsafeAllowGenericTargets**: When enabled, passes `--unsafe-allow-generic-targets=true` to the core controller, allowing ExternalSecret resources to sync into non-Secret Kubernetes resources (ConfigMaps, custom resources). The operator-managed `external-secrets-controller` ClusterRole/Binding does **not** grant write access to arbitrary target resource types; administrators must create additional RBAC for the `external-secrets` ServiceAccount when using this feature.

**Lifecycle note**: `CreateDefaultESMResource` runs at operator startup. If the ESM CR is deleted at runtime, the controller removes its finalizer but does not recreate the object until the operator process is restarted.

## Status

```go
type ExternalSecretsManagerStatus struct {
    ControllerStatuses  []ControllerStatus  // Aggregated from ESC conditions
    LastTransitionTime  metav1.Time         // Last condition change
}
```

**Note**: ESM uses a custom `Condition` type (Type, Status, Message) — NOT the full `metav1.Condition`. This is intentional per API linter comment.

### ControllerStatuses

Each entry represents a controller with its name, conditions, and `observedGeneration`.

## Lifecycle

1. **Creation**: Auto-created by operator at startup via `CreateDefaultESMResource` with retry logic (stops on AlreadyExists, Conflict, Invalid, BadRequest, Unauthorized, Forbidden, TooManyRequests)
2. **Update**: User modifies spec for global settings; controller updates status by aggregating ESC conditions
3. **Deletion**: Not user-managed — operator recreates on startup

## Example: Enable Generic Targets

```yaml
apiVersion: operator.openshift.io/v1alpha1
kind: ExternalSecretsManager
metadata:
  name: cluster
spec:
  features:
    - name: UnsafeAllowGenericTargets
      mode: Enabled
  globalConfig:
    logLevel: 2
    labels:
      environment: production
```

## Common Mistakes

1. **Name must be `cluster`** — singleton enforced via CEL
2. **Max 1 feature entry** — `MaxItems:=1` on the features list
3. **Don't delete ESM** — the operator recreates it; configure via spec instead
4. **UnsafeAllowGenericTargets needs RBAC** — enabling without granting the operand additional permissions causes reconciliation failures

## Related Concepts

- [ExternalSecretsConfig](./external-secrets-config.md) — Per-deployment configuration and installation trigger
