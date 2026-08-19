# ESO-481: Fix ConfigMap secondary watch predicate

## Problem

Secondary watches use label predicates to decide which operand/user resources
should enqueue `ExternalSecretsConfig` reconciliation.

`managedResources` already correctly checks both `ObjectOld` and `ObjectNew` on
update events (via inline `predicate.Funcs{}`). However,
`managedOrWatchedResources` — used for ConfigMaps including user
`trustedCABundle` references — is built with `predicate.NewPredicateFuncs()`,
which only inspects `ObjectNew`.

**Impact**: When a watched label is present on the old object but removed on the
new object, the event is filtered out before reconcile runs, breaking
self-healing for watched ConfigMaps.

## Root Cause

`predicate.NewPredicateFuncs(f)` creates a predicate where all event types
(Create, Update, Delete, Generic) pass `f` only the single object available.
For `UpdateFunc` specifically, it evaluates **only `ObjectNew`**, unlike the
hand-rolled `predicate.Funcs{}` block for `managedResources` that checks
`e.ObjectOld || e.ObjectNew`.

## Solution

1. **Extract `labelMatchPredicate` helper** in `utils.go` that accepts a
   `func(client.Object) bool` and returns a `predicate.Funcs` checking both
   old and new objects on updates.

2. **Extract package-level `isManagedResource` and `isManagedOrWatchedResource`**
   functions from the inline closure in `SetupWithManager`, making them
   reusable and testable.

3. **Refactor `controller.go`** to replace both the inline `predicate.Funcs{}`
   block AND `predicate.NewPredicateFuncs()` with calls to
   `labelMatchPredicate(isManagedResource)` and
   `labelMatchPredicate(isManagedOrWatchedResource)`.

4. **Add unit tests** verifying that the predicate correctly admits update
   events when:
   - Only old object has the label (label removed — the critical fix)
   - Only new object has the label (label added)
   - Both objects have the label
   - Neither object has the label (should filter)

## Files Modified

| File | Change |
|------|--------|
| `pkg/controller/external_secrets/utils.go` | Add `isManagedResource`, `isManagedOrWatchedResource`, `labelMatchPredicate` |
| `pkg/controller/external_secrets/controller.go` | Replace inline predicates with helper calls, remove unused `event` import |
| `pkg/controller/external_secrets/utils_test.go` | Add `TestLabelMatchPredicateUpdate` and `TestLabelMatchPredicateCreateDeleteGeneric` |

## Commit Plan

Single commit: `fix(controller): ESO-481 fix secondary watch predicates to evaluate old and new objects on update`
