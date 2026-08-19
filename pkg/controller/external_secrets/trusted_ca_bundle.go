package external_secrets

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
)

const (
	// trustedCABundleEventUnsupportedPEM is recorded when the bundle contains a non-CERTIFICATE PEM block.
	trustedCABundleEventUnsupportedPEM = "TrustedCABundleUnsupportedPEMBlock"
	// trustedCABundleEventNotCA is recorded when a PEM certificate is not a CA certificate.
	trustedCABundleEventNotCA = "TrustedCABundleNotCACertificate"
	// trustedCABundleEventPrivateKey is recorded when the bundle contains a private key PEM block.
	trustedCABundleEventPrivateKey = "TrustedCABundlePrivateKeyRejected"
	// trustedCABundleEventInvalidPEM is recorded for empty, malformed, or unparsable bundle data.
	trustedCABundleEventInvalidPEM = "InvalidTrustedCABundle"
	// trustedCABundleEventSkippedCNOProxy is recorded when user trustedCABundle is skipped because
	// the ConfigMap is CNO-managed and proxy TLS already uses the injected cluster CA bundle.
	trustedCABundleEventSkippedCNOProxy = "TrustedCABundleSkippedCNOProxy"
)

// trustedCABundleValidationError carries the Kubernetes event reason for a validation failure.
type trustedCABundleValidationError struct {
	eventReason string
	message     string
}

func (e *trustedCABundleValidationError) Error() string {
	return e.message
}

// validatePEMCABundleData checks that data contains at least one PEM-encoded X.509 CA certificate.
func validatePEMCABundleData(data string) error {
	return parsePEMCABundle(data)
}

// validateTrustedCABundleData validates bundle PEM and records warning events on ExternalSecretsConfig.
func (r *Reconciler) validateTrustedCABundleData(
	esc *operatorv1alpha1.ExternalSecretsConfig,
	cm types.NamespacedName,
	key, data string,
) error {
	if err := parsePEMCABundle(data); err != nil {
		r.recordTrustedCABundleValidationEvent(esc, cm, key, err)
		return err
	}
	r.now.Reset()
	return nil
}

// recordTrustedCABundleValidationEvent emits a Warning event with a reason derived from the validation error.
func (r *Reconciler) recordTrustedCABundleValidationEvent(
	esc *operatorv1alpha1.ExternalSecretsConfig,
	cm types.NamespacedName,
	key string,
	err error,
) {
	reason := trustedCABundleEventInvalidPEM
	var validationErr *trustedCABundleValidationError
	if errors.As(err, &validationErr) {
		reason = validationErr.eventReason
	}
	// Record at most one validation warning per degraded period. Reset when validation succeeds,
	// trustedCABundle is cleared, or the full reconcile succeeds (reconcileDeploymentSuccessResult).
	r.now.Do(func() {
		r.eventRecorder.Eventf(
			esc,
			corev1.EventTypeWarning,
			reason,
			"trustedCABundle ConfigMap %q key %q: %s",
			cm, key, err.Error(),
		)
	})
}

// parsePEMCABundle decodes PEM blocks and validates that at least one X.509 CA certificate is present.
// Each block must be a CERTIFICATE; private keys and leaf certificates are rejected.
func parsePEMCABundle(data string) error {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return &trustedCABundleValidationError{
			eventReason: trustedCABundleEventInvalidPEM,
			message:     "trusted CA bundle data is empty",
		}
	}

	var caCount int
	rest := []byte(trimmed)
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining

		if isPrivateKeyPEMBlock(block.Type) {
			return &trustedCABundleValidationError{
				eventReason: trustedCABundleEventPrivateKey,
				message:     fmt.Sprintf("trusted CA bundle must not contain private key PEM block type %q", block.Type),
			}
		}
		if block.Type != "CERTIFICATE" {
			return &trustedCABundleValidationError{
				eventReason: trustedCABundleEventUnsupportedPEM,
				message:     fmt.Sprintf("trusted CA bundle contains unsupported PEM block type %q", block.Type),
			}
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return &trustedCABundleValidationError{
				eventReason: trustedCABundleEventInvalidPEM,
				message:     fmt.Sprintf("invalid certificate in trusted CA bundle: %v", err),
			}
		}
		if !isCACertificate(cert) {
			return &trustedCABundleValidationError{
				eventReason: trustedCABundleEventNotCA,
				message:     fmt.Sprintf("certificate %q is not a CA certificate", cert.Subject.String()),
			}
		}
		caCount++
	}

	if caCount == 0 {
		return &trustedCABundleValidationError{
			eventReason: trustedCABundleEventInvalidPEM,
			message:     "trusted CA bundle contains no valid PEM-encoded CA certificates",
		}
	}
	if strings.TrimSpace(string(rest)) != "" {
		return &trustedCABundleValidationError{
			eventReason: trustedCABundleEventInvalidPEM,
			message:     "trusted CA bundle contains trailing non-PEM data",
		}
	}
	return nil
}

func isPrivateKeyPEMBlock(blockType string) bool {
	switch blockType {
	case "PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "DSA PRIVATE KEY":
		return true
	default:
		return false
	}
}

// isCACertificate reports whether cert may act as a trust anchor (IsCA or KeyUsageCertSign).
func isCACertificate(cert *x509.Certificate) bool {
	if cert.IsCA {
		return true
	}
	return cert.KeyUsage&x509.KeyUsageCertSign != 0
}
