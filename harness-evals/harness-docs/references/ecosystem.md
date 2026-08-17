# Platform Ecosystem References

This document links to generic OpenShift/Kubernetes patterns in the Platform ecosystem hub. The component inherits these platform-wide patterns and practices.

## Operator Patterns

**Location**: [openshift/enhancements/ai-docs/platform/operator-patterns/](https://github.com/openshift/enhancements/tree/master/ai-docs/platform/operator-patterns)

- **Controller Runtime**: Reconciliation loops, event handling, client patterns
- **Status Conditions**: Available, Progressing, Degraded condition semantics
- **Webhooks**: Validation and mutation patterns
- **Finalizers**: Resource cleanup patterns
- **RBAC**: Service account and permissions

**Component Usage**:
- Uses controller-runtime v0.23.3 (not library-go)
- Custom error classification: IrrecoverableError, RetryRequiredError, UserConfigurationError
- Standard `metav1.Condition` on ESC; custom lightweight `Condition` on ESM
- Finalizer-protected deletion on ExternalSecretsConfig

## Testing Practices

- **Test Pyramid**: Unit > Integration > E2E
- **E2E Framework**: OpenShift E2E test patterns

**Component Usage**:
- See `ESO_TESTING.md` for component-specific test suites
- Unit tests use counterfeiter fakes for client mocking
- API tests use envtest for CEL validation
- E2E tests use Ginkgo v2 with label-based filtering

## Security Practices

- **STRIDE Threat Model**: Threat modeling framework
- **RBAC Guidelines**: Role and ClusterRole design

**Component Usage**:
- All operand containers enforce restricted security context (drop ALL, read-only root, non-root, seccomp RuntimeDefault)
- Network policies isolate operand namespace with deny-all default
- HTTP/2 disabled on operator for security
- Reserved annotation/env var domains blocked via CEL

## Reliability Practices

- **SLO Framework**: Service Level Objectives and error budgets
- **Observability**: Metrics, logging, tracing patterns

**Component Usage**:
- Prometheus metrics endpoint with OpenShift service CA
- Error classification drives requeue behavior (30s for retryable, none for irrecoverable)
- Event recording for reconciliation outcomes

## Kubernetes Fundamentals

**Location**: [openshift/enhancements/ai-docs/domain/kubernetes/](https://github.com/openshift/enhancements/tree/master/ai-docs/domain/kubernetes)

- **Pod**: Pod lifecycle, container specs
- **CRDs**: CustomResourceDefinition patterns

**Component Usage**:
- Two cluster-scoped CRDs: ExternalSecretsConfig, ExternalSecretsManager
- Upstream external-secrets CRDs distributed via OLM bundle
- CEL validation rules for singleton enforcement and field immutability

## OpenShift Integrations

**Location**: [openshift/enhancements/ai-docs/domain/openshift/](https://github.com/openshift/enhancements/tree/master/ai-docs/domain/openshift)

**Component Usage**:
- **Trusted CA Bundle**: CNO injects cluster CA via `config.openshift.io/inject-trusted-cabundle` label
- **Proxy**: Falls back to OLM-injected HTTP_PROXY/HTTPS_PROXY/NO_PROXY env vars
- **Console**: QuickStart content for operator setup
- **Multi-arch**: NodeAffinity for amd64, arm64, ppc64le, s390x
- **OLM**: Operator distributed via OLM bundle; uses RELATED_IMAGE_* convention for disconnected support

## Cross-Repository ADRs

**Component-Specific ADRs**: See [`../decisions/`](../decisions/) for component-specific decisions.

---

**Note**: These links point to Platform (ecosystem hub) documentation. Component-specific patterns are documented under `harness-evals/harness-docs/` in this repository.

**Last Updated**: 2026-07-31
