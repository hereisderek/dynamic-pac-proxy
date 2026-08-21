// Package mitm generates dynamic-pac-proxy's own local certificate
// authority and issues per-hostname leaf certificates signed by it, so the
// proxy itself can terminate a client's TLS connection (see the host
// config's intercept_ssl) instead of just splicing opaque bytes end to
// end. The CA's public certificate is meant to be installed/trusted on
// client devices (served at /certs, alongside any manually-dropped-in
// Charles certificate); its private key must never leave this box.
package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CACommonName is the display name for the generated root CA — shown in
// the OS's certificate trust UI and on the /certs download page.
const CACommonName = "Dynamic PAC Proxy Local CA"

// CACertFileName and CAKeyFileName are the default filenames for the
// generated CA's public certificate (safe to publish — meant to be served
// under /certs) and its private signing key (never served; kept next to
// config.yaml instead — see config.Store.ConfigDir).
const (
	CACertFileName = "dynamic-pac-proxy-ca.pem"
	CAKeyFileName  = "dynamic-pac-proxy-ca-key.pem"
)

const (
	caValidity = 10 * 365 * 24 * time.Hour
	// leafValidity stays under the ~398 day cap browsers enforce for
	// publicly-trusted certs — harmless to also follow here even though
	// this CA is privately trusted, and it keeps leaf certs looking like
	// ones a real site would present.
	leafValidity = 397 * 24 * time.Hour
	clockSkew    = time.Hour
)

// CA is a locally-generated certificate authority used to sign per-host
// leaf certificates on demand.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// LoadOrCreate loads an existing CA from certPath/keyPath if both are
// present, parse, and not expired, or generates a fresh CA and writes it
// to those paths otherwise. certPath ends up served publicly (under
// /certs) so users can install/trust it; keyPath must never be served —
// it's the CA's private signing key.
func LoadOrCreate(certPath, keyPath string) (*CA, error) {
	if ca, err := load(certPath, keyPath); err == nil {
		return ca, nil
	}
	return create(certPath, keyPath)
}

func load(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("%s: not a PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", certPath, err)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, fmt.Errorf("%s: expired", certPath)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("%s: not a PEM key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}

	return &CA{cert: cert, key: key, cache: make(map[string]*tls.Certificate)}, nil
}

func create(certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: CACommonName, Organization: []string{"dynamic-pac-proxy"}},
		NotBefore:             time.Now().Add(-clockSkew),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse generated CA certificate: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", certDER, 0o644); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, err
	}

	return &CA{cert: cert, key: key, cache: make(map[string]*tls.Certificate)}, nil
}

func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// LeafCertificate returns a certificate for hostname, signed by this CA —
// generating and caching a fresh one on first request for that hostname so
// repeat connections to the same site don't pay for key generation again.
func (ca *CA) LeafCertificate(hostname string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if cert, ok := ca.cache[hostname]; ok {
		return cert, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key for %s: %w", hostname, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now().Add(-clockSkew),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("issue leaf certificate for %s: %w", hostname, err)
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{der, ca.cert.Raw},
		PrivateKey:  key,
	}
	ca.cache[hostname] = cert
	return cert, nil
}

// Certificate returns the CA's own public certificate — e.g. to verify a
// leaf certificate chains up to it, or to inspect its fields.
func (ca *CA) Certificate() *x509.Certificate {
	return ca.cert
}

// CertificateFor implements the logic behind tls.Config.GetCertificate:
// issue a leaf certificate for whatever hostname the client's ClientHello
// asked for via SNI, falling back to fallbackHost if the client didn't
// send SNI (rare, but not impossible for non-browser clients).
func (ca *CA) CertificateFor(hello *tls.ClientHelloInfo, fallbackHost string) (*tls.Certificate, error) {
	name := hello.ServerName
	if name == "" {
		name = fallbackHost
	}
	if name == "" {
		return nil, fmt.Errorf("no SNI hostname to issue a certificate for")
	}
	return ca.LeafCertificate(name)
}
