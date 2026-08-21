package mitm_test

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/derekhud/dynamic-pac-proxy/internal/mitm"
)

func TestLoadOrCreateGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	ca, err := mitm.LoadOrCreate(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if ca.Certificate().Subject.CommonName != mitm.CACommonName {
		t.Fatalf("CommonName = %q, want %q", ca.Certificate().Subject.CommonName, mitm.CACommonName)
	}
	if !ca.Certificate().IsCA {
		t.Fatal("generated certificate is not marked as a CA")
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("cert not written: %v", err)
	}
	if certInfo.Mode().Perm()&0o044 == 0 {
		t.Fatalf("cert file perms %v: expected it to be world/group readable, it's meant to be served publicly", certInfo.Mode())
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key not written: %v", err)
	}
	if keyInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key file perms %v: private key must not be group/world readable", keyInfo.Mode())
	}

	// A second call must reuse what's on disk, not silently mint a new CA
	// out from under already-installed client trust.
	reloaded, err := mitm.LoadOrCreate(certPath, keyPath)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if !reloaded.Certificate().Equal(ca.Certificate()) {
		t.Fatal("reloading LoadOrCreate produced a different certificate instead of reusing the persisted one")
	}
}

func TestLeafCertificateSignedByCAAndCached(t *testing.T) {
	dir := t.TempDir()
	ca, err := mitm.LoadOrCreate(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	leaf, err := ca.LeafCertificate("example.com")
	if err != nil {
		t.Fatalf("LeafCertificate: %v", err)
	}
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse issued leaf: %v", err)
	}
	if len(leafCert.DNSNames) != 1 || leafCert.DNSNames[0] != "example.com" {
		t.Fatalf("DNSNames = %v, want [example.com]", leafCert.DNSNames)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	if _, err := leafCert.Verify(x509.VerifyOptions{
		DNSName: "example.com",
		Roots:   pool,
	}); err != nil {
		t.Fatalf("issued leaf certificate doesn't verify against the CA: %v", err)
	}

	again, err := ca.LeafCertificate("example.com")
	if err != nil {
		t.Fatalf("second LeafCertificate: %v", err)
	}
	if again != leaf {
		t.Fatal("expected the second call for the same hostname to return the cached certificate, not mint a new one")
	}

	other, err := ca.LeafCertificate("other.example")
	if err != nil {
		t.Fatalf("LeafCertificate for a different hostname: %v", err)
	}
	if other == leaf {
		t.Fatal("expected a different hostname to get its own certificate")
	}
}

func TestCertificateForUsesSNIThenFallsBack(t *testing.T) {
	dir := t.TempDir()
	ca, err := mitm.LoadOrCreate(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	cert, err := ca.CertificateFor(&tls.ClientHelloInfo{ServerName: "sni.example"}, "fallback.example")
	if err != nil {
		t.Fatalf("CertificateFor with SNI: %v", err)
	}
	leafCert, _ := x509.ParseCertificate(cert.Certificate[0])
	if leafCert.DNSNames[0] != "sni.example" {
		t.Fatalf("expected SNI to take priority, got cert for %v", leafCert.DNSNames)
	}

	cert, err = ca.CertificateFor(&tls.ClientHelloInfo{}, "fallback.example")
	if err != nil {
		t.Fatalf("CertificateFor without SNI: %v", err)
	}
	leafCert, _ = x509.ParseCertificate(cert.Certificate[0])
	if leafCert.DNSNames[0] != "fallback.example" {
		t.Fatalf("expected the fallback host when SNI is empty, got cert for %v", leafCert.DNSNames)
	}

	if _, err := ca.CertificateFor(&tls.ClientHelloInfo{}, ""); err == nil {
		t.Fatal("expected an error when neither SNI nor a fallback host is available")
	}
}
