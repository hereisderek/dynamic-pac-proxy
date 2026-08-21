package mitm_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestLoadOrCreateDoesNotRegenerateOnCorruptFile guards against silently
// minting (and persisting) a brand new CA over a corrupted or otherwise
// unreadable one — that would invalidate trust already installed on
// client devices with no indication anything went wrong. Regeneration
// must be reserved for "doesn't exist yet" and "expired".
func TestLoadOrCreateDoesNotRegenerateOnCorruptFile(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	// A real key, but garbage in place of the certificate — e.g. a
	// truncated write or on-disk corruption, not a missing file.
	if _, err := mitm.LoadOrCreate(certPath, keyPath); err != nil {
		t.Fatalf("seed LoadOrCreate: %v", err)
	}
	corruptBytes := []byte("this is not a valid PEM certificate")
	if err := os.WriteFile(certPath, corruptBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mitm.LoadOrCreate(certPath, keyPath); err == nil {
		t.Fatal("expected LoadOrCreate to fail on a corrupted cert file, not silently regenerate")
	}

	stillCorrupt, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stillCorrupt) != string(corruptBytes) {
		t.Fatal("expected the corrupted cert file to be left untouched, not overwritten by a freshly minted CA")
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

// TestLeafCertificateIPLiteralUsesIPAddresses guards against a leaf
// certificate for an IP-literal SNI/host (e.g. https://127.0.0.1) putting
// that value in DNSNames — TLS verifiers require IP literals in
// IPAddresses instead, so a DNSNames-only cert fails hostname
// verification for such hosts.
func TestLeafCertificateIPLiteralUsesIPAddresses(t *testing.T) {
	dir := t.TempDir()
	ca, err := mitm.LoadOrCreate(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	for _, ipLiteral := range []string{"127.0.0.1", "::1"} {
		leaf, err := ca.LeafCertificate(ipLiteral)
		if err != nil {
			t.Fatalf("LeafCertificate(%q): %v", ipLiteral, err)
		}
		leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
		if err != nil {
			t.Fatalf("parse issued leaf for %q: %v", ipLiteral, err)
		}
		if len(leafCert.DNSNames) != 0 {
			t.Fatalf("DNSNames = %v, want none for IP literal %q", leafCert.DNSNames, ipLiteral)
		}
		if len(leafCert.IPAddresses) != 1 || !leafCert.IPAddresses[0].Equal(net.ParseIP(ipLiteral)) {
			t.Fatalf("IPAddresses = %v, want [%s]", leafCert.IPAddresses, ipLiteral)
		}

		pool := x509.NewCertPool()
		pool.AddCert(ca.Certificate())
		if _, err := leafCert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
			t.Fatalf("issued leaf for %q doesn't verify against the CA: %v", ipLiteral, err)
		}
	}
}

// TestLoadOrCreateRejectsInsecureKeyPermissions guards against trusting a
// CA private key that's readable by other users on the box — that breaks
// the whole point of keeping it off /certs. LoadOrCreate must fail loudly
// rather than silently loading (or regenerating over) it.
func TestLoadOrCreateRejectsInsecureKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	if _, err := mitm.LoadOrCreate(certPath, keyPath); err != nil {
		t.Fatalf("seed LoadOrCreate: %v", err)
	}

	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := mitm.LoadOrCreate(certPath, keyPath); err == nil {
		t.Fatal("expected LoadOrCreate to reject a group/world-readable private key file")
	}
}

// TestLoadOrCreateRejectsMismatchedKey guards against a restored/mixed-up
// cert+key pair that parses fine individually but doesn't actually belong
// together — that would start up successfully yet issue leaves no client
// could verify against the installed CA cert.
func TestLoadOrCreateRejectsMismatchedKey(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	if _, err := mitm.LoadOrCreate(certPath, keyPath); err != nil {
		t.Fatalf("seed LoadOrCreate: %v", err)
	}

	unrelatedKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	writeECKeyPEM(t, keyPath, unrelatedKey)

	if _, err := mitm.LoadOrCreate(certPath, keyPath); err == nil {
		t.Fatal("expected LoadOrCreate to reject a private key that doesn't match the certificate's public key")
	}
}

// TestLoadOrCreateRejectsNonCACertificate guards against loading a
// certificate that parses fine but was never actually a CA (missing
// BasicConstraints/IsCA) — accepting it would issue leaves under a
// "CA" that no client's trust store would ever recognize as one.
func TestLoadOrCreateRejectsNonCACertificate(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "not a real CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// IsCA/BasicConstraintsValid deliberately left false.
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	writeECKeyPEM(t, keyPath, key)

	if _, err := mitm.LoadOrCreate(certPath, keyPath); err == nil {
		t.Fatal("expected LoadOrCreate to reject a certificate that isn't a CA")
	}
}

func writeECKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
