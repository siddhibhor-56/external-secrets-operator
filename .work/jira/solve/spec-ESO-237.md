# ESO-237: Fix reconciliation blocked when managed labels are externally modified

## Problem

When the `app: external-secrets` label on a managed resource (e.g., ConfigMap
`external-secrets-trusted-ca-bundle`) is externally removed or changed, the
operator's reconcile loop enters a permanent `AlreadyExists` error cycle.

**Root Cause chain:**

1. Operator informer cache is filtered by `app=external-secrets`.
2. External actor removes the label from a managed resource.
3. Resource disappears from the label-filtered cache.
4. `r.Exists()` returns `false` (cache miss).
5. `r.Create()` is called — API server returns `AlreadyExists`.
6. Error is not handled; reconciler retries forever.

## Solution

Three complementary fixes:

### 1. `createWithFallback` — full-update fallback (operator-owned resources)

For resources whose entire spec is owned by the operator (Deployments, Services,
ServiceAccounts, NetworkPolicies, RBAC resources, Certificates,
ValidatingWebhookConfigurations):

- Wrap `Create()` calls: if the error is `AlreadyExists`, bypass the stale cache
  using `r.UncachedClient`, then perform `UpdateWithRetry` to restore labels,
  annotations, **and** spec to the desired state.
- Record an event noting the resource was restored.

### 2. `createWithMetadataFallback` — metadata-only patch (co-managed resources)

For resources where the data portion is managed externally (Secrets with
cert-controller-managed TLS data, trusted CA bundle ConfigMap with CNO-managed
data):

- On `AlreadyExists`, use `patchResourceMetadata` (JSON Patch) to replace only
  `metadata.labels` and `metadata.annotations`, leaving data untouched.

### 3. Watch predicate enhancement

Update the `managedResources` predicate to check **both** `ObjectOld` and
`ObjectNew` on update events. This ensures reconciliation still triggers when a
managed label is removed externally (the old object still carries the label even
though the new one does not).

## Files Changed

| File | Change |
|------|--------|
| `pkg/controller/external_secrets/utils.go` | Add `createWithFallback`, `createWithMetadataFallback`, `patchResourceMetadata`, `labelMatchPredicate` |
| `pkg/controller/external_secrets/controller.go` | Use new `labelMatchPredicate` in `SetupWithManager` |
| `pkg/controller/external_secrets/deployments.go` | Use `createWithFallback` on Create path |
| `pkg/controller/external_secrets/services.go` | Use `createWithFallback` on Create path |
| `pkg/controller/external_secrets/serviceaccounts.go` | Use `createWithFallback` on Create path |
| `pkg/controller/external_secrets/rbacs.go` | Use `createWithFallback` on Create paths |
| `pkg/controller/external_secrets/networkpolicy.go` | Use `createWithFallback` on Create paths |
| `pkg/controller/external_secrets/certificate.go` | Use `createWithFallback` on Create path |
| `pkg/controller/external_secrets/validatingwebhook.go` | Use `createWithFallback` on Create path |
| `pkg/controller/external_secrets/secret.go` | Use `createWithMetadataFallback` on Create path |
| `pkg/controller/external_secrets/configmap.go` | Inline `patchResourceMetadata` fallback on Create path |
| `pkg/controller/external_secrets/utils_test.go` (new) | Unit tests for new helpers |

## Testing

- Unit tests for `createWithFallback`, `createWithMetadataFallback`, `patchResourceMetadata`
- `make lint-fix && make verify && make test`
