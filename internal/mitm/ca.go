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
	"errors"
	"fmt"
	"math/big"
	"net"
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
	// maxLeafCacheEntries bounds how many per-hostname leaf certificates
	// are cached at once. Without a cap, an intercepting proxy that sees
	// arbitrary client-chosen SNI values (or a hostname whose cached leaf
	// has expired and keeps regenerating under a slightly different key
	// each time) would grow this map forever. The cache is small and
	// cheap to regenerate, so on overflow it's simplest to just drop the
	// whole thing and start over rather than implement real LRU eviction.
	maxLeafCacheEntries = 4096
)

// CA is a locally-generated certificate authority used to sign per-host
// leaf certificates on demand.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu    sync.Mutex
	cache map[string]leafCacheEntry
}

// leafCacheEntry pairs a cached leaf certificate with its expiry so
// LeafCertificate can tell a still-valid cache hit from a stale one — a
// long-running process would otherwise keep serving an expired
// certificate to every client past leafValidity.
type leafCacheEntry struct {
	cert     *tls.Certificate
	notAfter time.Time
}

// errRegenerate marks the only two load() failures that should trigger
// create() minting a fresh CA: the files simply don't exist yet, or the
// existing CA has expired. Any other failure (corrupt PEM, an unreadable
// key, a permissions problem, a partial write) is returned as-is by
// LoadOrCreate instead — silently regenerating on those would overwrite a
// CA that may already be trusted on client devices, with no indication
// anything went wrong.
var errRegenerate = errors.New("mitm: CA missing or expired")

// LoadOrCreate loads an existing CA from certPath/keyPath if both are
// present, parse, and not expired, or generates a fresh CA and writes it
// to those paths otherwise. certPath ends up served publicly (under
// /certs) so users can install/trust it; keyPath must never be served —
// it's the CA's private signing key.
func LoadOrCreate(certPath, keyPath string) (*CA, error) {
	ca, err := load(certPath, keyPath)
	if err == nil {
		return ca, nil
	}
	if errors.Is(err, errRegenerate) {
		return create(certPath, keyPath)
	}
	return nil, fmt.Errorf("load local CA: %w", err)
}

func load(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", certPath, errRegenerate)
		}
		return nil, fmt.Errorf("read %s: %w", certPath, err)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", keyPath, errRegenerate)
		}
		return nil, fmt.Errorf("stat %s: %w", keyPath, err)
	}
	// A group/world-readable private key means this CA's trust boundary is
	// already broken — anyone with access to that file can mint leaf
	// certificates for any hostname. Refuse to load it rather than
	// silently trusting a key that may already be compromised; the
	// operator has to notice and chmod it themselves.
	if perm := keyInfo.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s: mode %v is group/world-accessible; chmod 600 it before it can be trusted as a CA private key", keyPath, perm)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", keyPath, err)
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
		return nil, fmt.Errorf("%s: expired: %w", certPath, errRegenerate)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return nil, fmt.Errorf("%s: certificate is not a valid CA", certPath)
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return nil, fmt.Errorf("%s: not self-signed: %w", certPath, err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("%s: not a PEM key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !certPub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("%s and %s: certificate public key does not match the private key", certPath, keyPath)
	}

	return &CA{cert: cert, key: key, cache: make(map[string]leafCacheEntry)}, nil
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

	return &CA{cert: cert, key: key, cache: make(map[string]leafCacheEntry)}, nil
}

// writePEM writes a PEM-encoded file with the given permissions. perm is
// only honored by OpenFile when it actually creates the file — if path
// already exists (e.g. an expired CA being regenerated), OpenFile reuses
// its current mode regardless of perm, so it's chmod'd explicitly too.
// That matters most for the private key: it must never end up
// group/world-readable just because the file happened to pre-exist with a
// looser mode.
func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
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
	now := time.Now()
	if entry, ok := ca.cache[hostname]; ok && now.Before(entry.notAfter) {
		return entry.cert, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key for %s: %w", hostname, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(leafValidity)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// TLS verifiers require an IP-literal SNI/host (e.g. https://127.0.0.1
	// or an IPv6 literal) to appear in IPAddresses, not DNSNames — a
	// DNSNames-only cert fails hostname verification for those.
	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{hostname}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("issue leaf certificate for %s: %w", hostname, err)
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{der, ca.cert.Raw},
		PrivateKey:  key,
	}
	if len(ca.cache) >= maxLeafCacheEntries {
		ca.cache = make(map[string]leafCacheEntry, maxLeafCacheEntries)
	}
	ca.cache[hostname] = leafCacheEntry{cert: cert, notAfter: notAfter}
	return cert, nil
}

// Certificate returns the CA's own public certificate — e.g. to verify a
// leaf certificate chains up to it, or to inspect its fields.
func (ca *CA) Certificate() *x509.Certificate {
	return ca.cert
}

// PublishCert (re-)writes ca's public certificate to certPath — used to
// move an already-loaded CA's public cert to a new location after
// certs_dir is hot-reloaded, without regenerating the CA (and so without
// invalidating trust already installed on client devices) or touching the
// private key at all.
func PublishCert(ca *CA, certPath string) error {
	return writePEM(certPath, "CERTIFICATE", ca.cert.Raw, 0o644)
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
