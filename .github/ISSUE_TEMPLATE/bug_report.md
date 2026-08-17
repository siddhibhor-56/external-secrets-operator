---
name: Bug Report
about: Report a bug in the External Secrets Operator
labels: kind/bug
---

## Describe the Bug

<!-- A clear description of the bug. -->

## Steps to Reproduce

1. 
2. 
3. 

## Expected Behavior

<!-- What you expected to happen. -->

## Actual Behavior

<!-- What actually happened. Include error messages or logs if available. -->

## Environment

- OpenShift / Kubernetes version:
- Operator version (`oc get csv -n external-secrets-operator`):
- External Secrets operand version:
- Secret provider (AWS, Vault, Azure, GCP, etc.):

## Relevant Resources

> **Redact** secret values, credentials, tokens, private keys, and personal data before posting resources or logs in a public issue.

```yaml
# oc get esc cluster -o yaml
```

```yaml
# oc get esm cluster -o yaml
```

## Operator Logs

```text
# oc logs -n external-secrets-operator deployment/external-secrets-operator-controller-manager
```

## Additional Context

<!-- Any other context: screenshots, related issues, workarounds tried. -->
