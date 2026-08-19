# Error Handling Guidelines

## Error Type Classification

This operator uses three `ReconcileError` categories defined in `pkg/controller/common/errors.go`. Every error returned from reconciliation sub-functions must be wrapped in one of these.

### IrrecoverableError

Permanent failures that no retry can fix. The reconciler does **not** requeue.

Use when:
- Required environment variables are missing (e.g., operand image env vars)
- An optional CRD is referenced in spec but not installed on the cluster
- The Kubernetes API returns Unauthorized, Forbidden, Invalid, or BadRequest

```go
common.NewIrrecoverableError(
    fmt.Errorf("%s environment variable not set", envVar),
    "failed to update image in %s deployment object", name)
```

### RetryRequiredError

Transient failures that may self-resolve. The reconciler requeues after `DefaultRequeueTime` (30s).

Use when:
- Network timeouts, API server unavailability, resource conflicts
- Any Kubernetes API error not classified as irrecoverable by `FromClientError`

### UserConfigurationError

The user's CR spec references something invalid or missing. Sets `Degraded=True`.

Use when:
- A referenced ConfigMap/Secret does not exist or lacks the expected key
- PEM data is malformed or contains non-CA certificates
- Proxy URL is invalid

Requeue behavior depends on the root cause:
- **NotFound** (`IsUserConfigurationNotFound`): requeue after 30s (the missing object has no watch yet)
- **All other user config errors**: do not requeue; recovery is driven by watches

## FromClientError Auto-Classification

For Kubernetes API client errors, use `common.FromClientError` instead of manually choosing a category:

| API Error | Mapped Reason |
|---|---|
| Unauthorized, Forbidden, Invalid, BadRequest | `IrrecoverableError` |
| NotFound, Conflict, Timeout, ServiceUnavailable, all others | `RetryRequiredError` |

**When to override**: If a NotFound error reflects user misconfiguration (not infrastructure), wrap it as `NewUserConfigurationError` instead of calling `FromClientError`.

## Status Condition Patterns

| Condition | When True | When False |
|---|---|---|
| `Ready` | Operand deployed and healthy | Reconciliation failed or in progress |
| `Degraded` | Irrecoverable or user-config error | Normal operation |
| `UpdateAnnotation` | CRD annotations updated | Annotation update failed |

Rules:
- On **IrrecoverableError** or **UserConfigurationError**: `Degraded=True`, `Ready=False`, `Reason=Failed`
- On **RetryRequiredError**: `Degraded=False` (Reason=Ready), `Ready=False` (Reason=Progressing)
- On **success**: `Degraded=False`, `Ready=True`, both `Reason=Ready`
- Always set `ObservedGeneration` from the CR's generation
- Only call `StatusUpdate` when a condition actually changed

## Requeue Decision Matrix

| Error Type | Requeue? | Returned Result |
|---|---|---|
| `IrrecoverableError` | No | `ctrl.Result{}, errUpdate` |
| `UserConfigurationError` (NotFound) | Yes, 30s | `ctrl.Result{RequeueAfter: DefaultRequeueTime}, nil` |
| `UserConfigurationError` (other) | No | `ctrl.Result{}, nil` |
| `RetryRequiredError` | Yes, 30s | `ctrl.Result{RequeueAfter: DefaultRequeueTime}, nil` |
| Success | No | `ctrl.Result{}, nil` |
| Status update failure | Yes (backoff) | `ctrl.Result{}, errUpdate` |

## Error Wrapping Conventions

1. **Prefer `%w` in `fmt.Errorf`** to preserve the error chain for `errors.Is`/`errors.As`. Simple error creation without wrapping is acceptable when no underlying error needs to be preserved.
2. **ReconcileError constructors wrap automatically** — do not double-wrap.
3. **Nil-safe constructors**: All `New*Error` and `FromClientError` return nil when input error is nil.
4. **Aggregate errors** for status update failures using `utilerrors.NewAggregate`.

## RetryOnConflict Patterns

### Status Updates

Use `retry.RetryOnConflict` with `retry.DefaultRetry`. Re-fetch the latest object, deep-copy desired status, then call `StatusUpdate`.

### Resource Updates (`UpdateWithRetry`)

Re-fetches the object for the latest `resourceVersion` before each update attempt.

### ESM Resource Creation

`CreateDefaultESMResource` uses `retry.OnError` with `shouldRetryOnError` that stops on: AlreadyExists, Conflict, Invalid, BadRequest, Unauthorized, Forbidden, TooManyRequests.

## Event Recording

Events are recorded on the `ExternalSecretsConfig` CR using `r.eventRecorder.Eventf`.

| Scenario | EventType | Reason |
|---|---|---|
| Resource created or updated | `Normal` | `Reconciled` |
| Resource already exists (adopted) | `Warning` | `ResourceAlreadyExists` |
| CR marked for deletion | `Warning` | `RemoveDeployment` |
| Trusted CA validation failure | `Warning` | Specific reason |

### Throttled Validation Events

`trusted_ca_bundle.go` uses `r.now.Do(f)` to emit at most one warning per degraded period. Resets on successful validation or when `trustedCABundle` is cleared.

## Sub-Function Error Return

Sub-functions in `install_external_secrets.go` return typed `ReconcileError` values. The top-level `reconcileExternalSecretsDeployment` propagates the first error and stops. When a sub-function encounters a user-config NotFound that should not block the entire deployment, it returns the deployment alongside the error for partial spec application.

## Testing Error Classification

Test with `Is*` helpers, not string-matching:

```go
if !common.IsUserConfigurationError(err) {
    t.Fatal("expected UserConfigurationError")
}
```
