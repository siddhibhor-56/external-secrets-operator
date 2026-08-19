# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in the External Secrets Operator for Red Hat OpenShift, please report it responsibly. **Do not open a public GitHub issue for security vulnerabilities.**

### Contact

Email: **external-secrets-oape@redhat.com**

### What to Include

- Description of the vulnerability
- Steps to reproduce
- Affected versions (operator and/or operand)
- Potential impact
- Any suggested fix or mitigation

### Response

- You will receive an acknowledgement within 3 business days.
- We will work with you to understand the issue and coordinate a fix.
- We will provide credit in the advisory unless you prefer to remain anonymous.

## Supported Versions

Security fixes are applied to the latest release branch. Refer to the [Red Hat External Secrets Operator documentation](https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/security_and_compliance/external-secrets-operator-for-red-hat-openshift) for supported OpenShift versions.

## Security Practices

This operator enforces several security controls documented in [harness-evals/harness-docs/security-guidelines.md](harness-evals/harness-docs/security-guidelines.md), including:

- Hardened container security contexts (non-root, read-only filesystem, dropped capabilities)
- CEL validation on CRD fields to prevent misconfiguration
- Deny-all-first network policy model for the operand namespace
- RBAC least-privilege with auto-generated ClusterRoles
- PEM validation rejecting private keys and non-CA certificates
- Reserved annotation/label/env-var domain blocking
