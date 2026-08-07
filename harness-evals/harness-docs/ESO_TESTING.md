# External Secrets Operator — Testing Guide

This guide covers **ESO-specific** test suites and patterns.

## Test Organization

```text
pkg/controller/external_secrets/*_test.go   # Unit tests (controller logic)
test/apis/                                   # API integration tests (CEL validation via envtest)
test/e2e/                                    # E2E tests (real OpenShift cluster)
test/utils/                                  # Shared test utilities
```

## Unit Tests

### Running Unit Tests

```bash
make test                    # All non-e2e tests (manifests, generate, fmt, vet, test-apis, test-unit)
make test-unit               # Unit tests only
go test -v ./pkg/...         # Specific package
go test -count=1 ./pkg/...   # Disable cache
go test -cover ./pkg/...     # With coverage
```

### Patterns

**Client mocking**: Uses [counterfeiter](https://github.com/maxbrunsfeld/counterfeiter)-generated fakes at `pkg/controller/client/fakes/fake_ctrl_client.go`.

```go
// Regenerate fakes after CtrlClient interface changes:
//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
```

**Controller tests** (`pkg/controller/external_secrets/*_test.go`):
- Test individual reconciliation functions (deployment creation, RBAC, network policies, etc.)
- Use fake clients to simulate cluster state
- Verify resource field values, label/annotation application, error classification

## API Integration Tests (envtest)

### Running API Tests

```bash
make test-apis                                    # Via Makefile
go test -v ./test/apis/... -ginkgo.v             # Direct
```

### Pattern

Uses controller-runtime's `envtest.Environment` to run a real API server with CRDs installed. Tests are **data-driven** via `.testsuite.yaml` files:

```go
// test/apis/suite_test.go
testEnv = &envtest.Environment{
    CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
}
```

**Purpose**: Validates CEL rules, default values, and field validation. Requires Kubernetes 1.25+ for CEL support.

**Test data**: `.testsuite.yaml` files live under `api/v1alpha1/tests/<crdname>.<group>/` (e.g., `externalsecretsconfig.operator.openshift.io/`), loaded by `LoadTestSuiteSpecs` and executed by `GenerateTestSuite`.

## E2E Tests

### Running E2E Tests

```bash
# Requires real OpenShift cluster with KUBECONFIG set
make test-e2e

# With specific labels
E2E_GINKGO_LABEL_FILTER="Platform:Generic && Feature:NetworkPolicy" make test-e2e

# Default filter excludes: Proxy, Upgrade, Bitwarden tests
```

**Build tag**: `//go:build e2e` — E2E tests are excluded from `make test`.

### Ginkgo Label System

| Label | Description |
|-------|-------------|
| `Platform:AWS` | Requires AWS cluster |
| `Platform:GCP` | Requires GCP cluster |
| `Platform:Generic` | Any cluster |
| `Provider:AWS` | Tests AWS Secrets Manager integration |
| `Provider:Vault` | Tests HashiCorp Vault integration |
| `Feature:NetworkPolicy` | Network policy lifecycle |
| `Feature:Proxy` | Proxy egress policy lifecycle |
| `Feature:OverrideEnv` | Per-component env var overrides |
| `Feature:RevisionHistoryLimit` | Deployment history configuration |
| `Feature:UnsafeAllowGenericTargets` | ESM feature toggle |
| `Feature:CustomAnnotations` | Annotation lifecycle and restoration |
| `Feature:CustomLabels` | Label lifecycle and restoration |
| `Skipped:Disconnected` | Excluded in disconnected environments |

### Test Suite Setup

```go
// test/e2e/e2e_suite_test.go
BeforeSuite: creates test namespace, initializes 3 clients (clientset, dynamic, runtime)
AfterSuite: dumps artifacts to ARTIFACT_DIR on failure, cleans up
```

### E2E Test Scenarios

**Secret Store lifecycle** (`e2e_test.go`):
- AWS: SecretStore → ExternalSecret → verify Secret data → PushSecret → ClusterSecretStore
- Vault: SecretStore with token auth → ExternalSecret → verify sync

**Drift detection** (`e2e_test.go`):
- Modify managed resource annotations/labels → verify operator restores them
- Per resource type: SA, Role, RoleBinding, ClusterRole, ClusterRoleBinding, Service, NetworkPolicy
- Deployment intentionally excluded from annotation restoration due to revision annotation churn

**Feature toggles** (`e2e_test.go`):
- Enable/disable `UnsafeAllowGenericTargets` → verify container args on core deployment

**Bitwarden** (`bitwarden_*.go`):
- Plugin lifecycle with cert-manager integration
- API-based bitwarden tests with secret sync verification

**Trusted CA Bundle** (`trusted_ca_bundle_test.go`):
- User-specified CA bundle ConfigMap → verify volume mount and SSL_CERT_DIR env var

### Test Utilities (`test/utils/`)

| Utility | Purpose |
|---------|---------|
| `conditions.go` | `WaitForExternalSecretsConfigReady`, pod readiness polling |
| `external_secrets_config.go` | Cert-manager detection, operand pod prefix computation |
| `dynamic_resources.go` | Load YAML with pattern replacement |
| `aws_resources.go` | AWS credential fetching, secret management |
| `bitwarden_resources.go` | Bitwarden test resource management |
| `artifact_dump.go` | Failure artifact collection to `ARTIFACT_DIR` |
| `cleanup.go` | Test namespace and resource cleanup |
| `kube_client.go` | Pod exec, architecture detection, vault image selection |

### Embedded Test Data

```go
//go:embed testdata
var testData embed.FS
```

Test YAML manifests (SecretStores, ExternalSecrets, etc.) are embedded and loaded via `dynamic_resources.go`.

## CI Configuration

- `.ci-operator.yaml` — CI operator configuration
- Unit tests and API tests run on every PR
- E2E tests run on target clusters with appropriate labels

## Debugging Test Failures

```bash
# E2E artifacts
ls $ARTIFACT_DIR/

# Operator logs
oc logs -n external-secrets-operator deployment/external-secrets-operator-controller-manager

# Operand status
oc get esc cluster -o yaml
oc get esm cluster -o yaml
oc get pods -n external-secrets
```

## See Also

- [Development Guide](./ESO_DEVELOPMENT.md)
- [Architecture](./architecture/components.md)
