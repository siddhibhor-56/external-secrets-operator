# Performance Guidelines

Conventions specific to the external-secrets-operator codebase for keeping the operator fast and stable at scale.

## 1. Cache Configuration

### 1.1 Label-filtered cache (managed resources)

The manager cache is configured in `NewCacheBuilder()` with a per-type label selector (`app=external-secrets`) for label-filtered operand resources listed in `controllerManagedResources`. ConfigMaps are the exception and use namespace scoping (see below). Only matching objects are stored in the informer cache for label-filtered types.

Rules:
- Every new operand resource type MUST be added to `controllerManagedResources` with the `app=external-secrets` label applied via `ApplyResourceMetadata` (unless it follows the ConfigMap namespace-scoped pattern).
- Never create an unfiltered cache entry for a type with unbounded cluster-wide cardinality (Secrets, ConfigMaps, ClusterRoles).
- Own CRs (`ExternalSecretsConfig`, `ExternalSecretsManager`) are cached without a label filter.

### 1.2 Namespace-scoped ConfigMap cache

ConfigMaps use namespace scoping (`OperandDefaultNamespace`) instead of a label selector because user-provided ConfigMaps only receive the watch label during reconciliation. Do not add a label selector to the ConfigMap cache entry.

### 1.3 AlreadyExists handling (`createWithFallback`)

A label-filtered cache can miss objects whose managed label was externally removed. `createWithFallback` handles the resulting `AlreadyExists` by falling back to `UncachedClient.UpdateWithRetry`. For externally-managed data (Secrets, ConfigMaps), use `createWithMetadataFallback` or `patchResourceMetadata` to touch only labels and annotations.

## 2. Change Detection

### 2.1 Field-level comparison (`HasObjectChanged`)

`HasObjectChanged` in `pkg/controller/common/utils.go` compares only operator-managed fields per resource type. When adding a new managed resource type, add a case to the switch with a type-specific comparator. Never compare `metadata.resourceVersion`, `status`, or `metadata.managedFields`.

### 2.2 Order-insensitive comparisons

Environment variables and volume mounts are compared order-insensitively using `slicesEqualUnordered` (clone, sort, then `DeepEqual`). Any new slice field whose ordering is not semantically significant must use this pattern.

### 2.3 Managed-key annotation comparison

`ObjectMetadataModified` and `annotationMapsModified` compare only operator-managed annotation keys. Never use `reflect.DeepEqual` on annotation maps — always pass through `ObjectMetadataModified` to avoid infinite reconcile loops.

## 3. Event Predicates and Watch Filtering

| Resource Type | Predicate | Reason |
|---|---|---|
| ExternalSecretsConfig (primary) | `GenerationChangedPredicate` | Skip status-only updates |
| Deployments | `GenerationChangedPredicate` OR `LabelChangedPredicate` | Filter pod rollout updates, catch label removals |
| Secrets | `WatchesMetadata` + `LabelChangedPredicate` | Avoid caching Secret data |
| ConfigMaps | `ResourceVersionChangedPredicate` | ConfigMaps lack `.metadata.generation` |

The `mapFunc` in `SetupWithManager` checks `hasManagedOrWatchLabel` before returning a reconcile request — objects without the managed or watch label produce an empty request slice.

Rule: Always pick the narrowest predicate. Use `GenerationChangedPredicate` for spec-bearing resources, `LabelChangedPredicate` for metadata-only watches, `ResourceVersionChangedPredicate` only when generation is unavailable.

## 4. Requeue Strategy

`DefaultRequeueTime` is 30 seconds (defined in `pkg/controller/common/constants.go`).

| Error Type | Requeue? | Rationale |
|---|---|---|
| `IrrecoverableError` | No | Permanent failure, no point spinning |
| `RetryRequiredError` | 30s | Transient, may self-resolve |
| `UserConfigurationError` (NotFound) | 30s | No watch events for nonexistent objects |
| `UserConfigurationError` (other) | No | Recovery is watch-driven |
| Success | No | Done |

`FromClientError` in `pkg/controller/common/errors.go` classifies API errors automatically: Unauthorized, Forbidden, Invalid, BadRequest become `IrrecoverableError`; all other client errors (including `NotFound`) become `RetryRequiredError`. Call sites that treat a missing user-referenced object as bad configuration should wrap with `NewUserConfigurationError` instead (see [`error-handling-guidelines.md`](error-handling-guidelines.md) for status/requeue behavior).

## 5. Concurrency Patterns

### 5.1 Resettable sync.Once (`common.Now`)

`Now` uses double-checked locking (`atomic.Uint32` + `sync.Mutex`) to call a function at most once per degraded period. Use `r.now.Do(f)` for events that should fire at most once per error period. Call `r.now.Reset()` when the error condition clears.

### 5.2 UpdateWithRetry / RetryOnConflict

`UpdateWithRetry` in `pkg/controller/client/client.go` wraps `retry.RetryOnConflict(retry.DefaultRetry, ...)`, re-fetching the object for the latest `resourceVersion` before each attempt.

Rules:
- Use `UpdateWithRetry` for all metadata/spec updates on shared objects.
- Prefer `Patch` over `Update` when touching a small number of fields.

### 5.3 Cache fallback reads

`getWithCacheFallback` reads from the manager cache first and falls back to `UncachedClient` on `IsNotFound`. Use only for resources that may not yet be in the label-filtered cache.

## 6. Status Update Efficiency

### 6.1 Skip unchanged conditions

Both `reconcileDeploymentSuccessResult` and `updateStatusConditionsOnFailure` check whether conditions actually changed before issuing a status update API call. Always gate on condition-change booleans — unconditional status writes generate unnecessary API traffic.

### 6.2 CR annotation patching

`updateCRAnnotationsIfNeeded` uses `MergePatch` for annotation-only updates. Never use full `Update` on the CR just to change an annotation.

## 7. Anti-Patterns to Avoid

1. **Full-object DeepEqual for change detection** — use `HasObjectChanged` or `ObjectMetadataModified`
2. **Bare Create without AlreadyExists fallback** — use `createWithFallback` or `createWithMetadataFallback`
3. **Unfiltered cache for cluster-scoped types** — add label or namespace filters
4. **Requeuing irrecoverable errors** — check `IsIrrecoverableError` and return without requeue
5. **Unconditional status writes** — gate on condition-change booleans
6. **Full Update on externally-managed resources** — use metadata-only patches for Secrets and ConfigMaps whose data is owned by other controllers
