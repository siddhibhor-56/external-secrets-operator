## Description

### What changed?
<!-- Summarize the changes made in this PR. -->

### Why?
<!-- What problem does this solve? What motivated this change? -->

### How?
<!-- Briefly describe the approach taken. Mention key design decisions, trade-offs, or alternatives considered. -->

## Type of Change

- [ ] Bug fix
- [ ] New feature
- [ ] CRD / API change
- [ ] Refactoring (no functional change)
- [ ] Documentation
- [ ] CI / build

## Checklist

- [ ] `make verify` passes (vet, fmt, deps, bindata, generated files, govulncheck, git diff)
- [ ] `make test` passes (unit + API integration tests)
- [ ] `make lint` passes
- [ ] New/changed CRD fields have appropriate CEL validation; add `.testsuite.yaml` tests for new CEL rules
- [ ] New managed resources added to `controllerManagedResources`, `buildCacheObjectList()`, `HasObjectChanged`, and the ordered install sequence
- [ ] No hand-edits to generated files (`bindata.go`, `zz_generated.deepcopy.go`, CRD YAML, fakes)
- [ ] Error paths use the correct error type (`IrrecoverableError` / `RetryRequiredError` / `UserConfigurationError`)

## Testing

<!-- How was this tested? Which make targets were run? For E2E, which label filter? -->

## Additional Context

<!-- Screenshots, logs, related PRs, or anything reviewers should know. -->
