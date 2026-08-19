package external_secrets

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	operatorv1alpha1 "github.com/openshift/external-secrets-operator/api/v1alpha1"
	"github.com/openshift/external-secrets-operator/pkg/controller/common"
)

func mustPEMCert(t *testing.T, isCA bool) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test-cert",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		IsCA:      false,
	}
	if isCA {
		template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		template.IsCA = true
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func testTrustedCABundleReconciler() *Reconciler {
	return &Reconciler{
		ctx:           context.Background(),
		eventRecorder: record.NewFakeRecorder(10),
		now:           &common.Now{},
	}
}

func assertRecorderWarningEvent(t *testing.T, r *Reconciler, reason string) {
	t.Helper()
	rec := r.eventRecorder.(*record.FakeRecorder)
	select {
	case evt := <-rec.Events:
		if !strings.Contains(evt, corev1.EventTypeWarning) || !strings.Contains(evt, reason) {
			t.Fatalf("event = %q, want Warning containing %q", evt, reason)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected Warning event with reason %q", reason)
	}
}

func assertNoRecorderEvent(t *testing.T, r *Reconciler) {
	t.Helper()
	rec := r.eventRecorder.(*record.FakeRecorder)
	select {
	case evt := <-rec.Events:
		t.Fatalf("unexpected event: %q", evt)
	default:
	}
}

func assertRecorderNormalEvent(t *testing.T, r *Reconciler, reason string) {
	t.Helper()
	rec := r.eventRecorder.(*record.FakeRecorder)
	select {
	case evt := <-rec.Events:
		if !strings.Contains(evt, corev1.EventTypeNormal) || !strings.Contains(evt, reason) {
			t.Fatalf("event = %q, want Normal containing %q", evt, reason)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected Normal event with reason %q", reason)
	}
}

func TestValidateTrustedCABundleData(t *testing.T) {
	t.Parallel()

	validCA := mustPEMCert(t, true)
	validLeaf := mustPEMCert(t, false)

	privateKeyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: []byte("not-a-real-key"),
	}))

	esc := &operatorv1alpha1.ExternalSecretsConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}
	cm := types.NamespacedName{Name: "user-ca", Namespace: OperandDefaultNamespace}
	key := UserCABundleKeyPath

	tests := []struct {
		name         string
		data         string
		wantErr      bool
		wantReason   string
		assertEvents func(t *testing.T, r *Reconciler)
	}{
		{
			name: "empty", data: "",
			wantErr:    true,
			wantReason: trustedCABundleEventInvalidPEM,
		},
		{
			name:       "comment only",
			data:       "# not a cert",
			wantErr:    true,
			wantReason: trustedCABundleEventInvalidPEM,
		},
		{
			name:       "invalid pem",
			data:       "not pem",
			wantErr:    true,
			wantReason: trustedCABundleEventInvalidPEM,
		},
		{
			name:    "valid single CA",
			data:    validCA,
			wantErr: false,
		},
		{
			name:    "two CAs",
			data:    validCA + "\n" + validCA,
			wantErr: false,
		},
		{
			name:    "valid CA with trailing whitespace",
			data:    validCA + "\n\n",
			wantErr: false,
		},
		{
			name:       "valid CA with trailing non-PEM garbage",
			data:       validCA + "\nthis is not pem",
			wantErr:    true,
			wantReason: trustedCABundleEventInvalidPEM,
		},
		{
			name:       "leaf certificate",
			data:       validLeaf,
			wantErr:    true,
			wantReason: trustedCABundleEventNotCA,
		},
		{
			name:       "CA and leaf",
			data:       validCA + "\n" + validLeaf,
			wantErr:    true,
			wantReason: trustedCABundleEventNotCA,
		},
		{
			name:       "private key block",
			data:       privateKeyPEM,
			wantErr:    true,
			wantReason: trustedCABundleEventPrivateKey,
		},
		{
			name:       "unsupported PEM block",
			data:       "-----BEGIN FOO-----\nYmFy\n-----END FOO-----",
			wantErr:    true,
			wantReason: trustedCABundleEventUnsupportedPEM,
		},
		{
			name:       "validation warning recorded once until reset",
			data:       "not pem",
			wantErr:    true,
			wantReason: trustedCABundleEventInvalidPEM,
			assertEvents: func(t *testing.T, r *Reconciler) {
				assertRecorderWarningEvent(t, r, trustedCABundleEventInvalidPEM)
				if err := r.validateTrustedCABundleData(esc, cm, key, "not pem"); err == nil {
					t.Fatal("expected validation error")
				}
				assertNoRecorderEvent(t, r)
				r.now.Reset()
				if err := r.validateTrustedCABundleData(esc, cm, key, "not pem"); err == nil {
					t.Fatal("expected validation error")
				}
				assertRecorderWarningEvent(t, r, trustedCABundleEventInvalidPEM)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := testTrustedCABundleReconciler()
			err := r.validateTrustedCABundleData(esc, cm, key, tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTrustedCABundleData() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var validationErr *trustedCABundleValidationError
				if !errors.As(err, &validationErr) {
					t.Fatalf("expected *trustedCABundleValidationError, got %T: %v", err, err)
				}
				if tt.wantReason != "" && validationErr.eventReason != tt.wantReason {
					t.Fatalf("event reason = %q, want %q", validationErr.eventReason, tt.wantReason)
				}
			}
			switch {
			case tt.assertEvents != nil:
				tt.assertEvents(t, r)
			case tt.wantErr && tt.wantReason != "":
				assertRecorderWarningEvent(t, r, tt.wantReason)
			default:
				assertNoRecorderEvent(t, r)
			}
		})
	}
}

func TestValidatePEMCABundleData(t *testing.T) {
	t.Parallel()

	validCA := mustPEMCert(t, true)

	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{
			name:    "valid CA",
			data:    validCA,
			wantErr: false,
		},
		{
			name:    "invalid pem",
			data:    "not pem",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validatePEMCABundleData(tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePEMCABundleData() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIsCACertificate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		isCA   bool
		wantCA bool
	}{
		{
			name:   "CA certificate",
			isCA:   true,
			wantCA: true,
		},
		{
			name:   "leaf certificate",
			isCA:   false,
			wantCA: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cert := mustParseCert(t, mustPEMCert(t, tt.isCA))
			if got := isCACertificate(cert); got != tt.wantCA {
				t.Fatalf("isCACertificate() = %v, want %v", got, tt.wantCA)
			}
		})
	}
}

func mustParseCert(t *testing.T, pemData string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		t.Fatal("failed to decode PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}
