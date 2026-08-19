# Fix ConfigMap secondary watch predicate

## Problem

Secondary watches use label predicates to decide which operand/user resources
should enqueue `ExternalSecretsConfig` reconciliation.

`managedResources` already checked both `ObjectOld` and `ObjectNew` on updates
via `labelMatchPredicate`, but **test coverage only covered Update events**.
Create, Delete, and Generic event paths for both the managed-only and
managed-or-watched predicates were untested.

The core code fix (extracting `labelMatchPredicate` and using it for both
`managedResources` and `managedOrWatchedResources`) was applied in prior PRs
(ESO-237 / #153, ESO-481 / #158). This change adds the missing test coverage.

## Solution

### Already in place (no code changes needed)

- `labelMatchPredicate(match)` helper in `utils.go` checks both `ObjectOld`
  and `ObjectNew` on Update events.
- Both `managedResources` and `managedOrWatchedResources` in
  `SetupWithManager` use `labelMatchPredicate`.
- `isManagedResource` and `isManagedOrWatchedResource` label matchers are
  consolidated in `utils.go`.

### Added in this change

Comprehensive unit tests for `labelMatchPredicate` covering:

- **Create events**: with managed label, watched label, both, neither
- **Delete events**: with managed label, watched label, both, neither
- **Generic events**: with managed label, watched label, both, neither
- Each event type tested against both `isManagedResource` and
  `isManagedOrWatchedResource` matchers

## Files changed

| File | Change |
|------|--------|
| `pkg/controller/external_secrets/utils_test.go` | Add `TestLabelMatchPredicateCreateDeleteGeneric` |

## Verification

```bash
make lint-fix
make verify
make test
```
