package edge_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/addon"
	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/edge"
)

// selfSignedTLSConfig stands in for what a real certmagic.Config.TLSConfig()
// would return — a throwaway, short-lived cert, so tests can exercise the
// reverse-proxy/addon pipeline with zero ACME/network involvement.
func selfSignedTLSConfig(domain string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, nil
}

func fakeCertFunc() edge.CertFunc {
	return func(ctx context.Context, serve *config.SiteServe) (*tls.Config, error) {
		return selfSignedTLSConfig(serve.Domain)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeScript(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeConfigYAML hand-writes a minimal valid config.yaml — one dummy host
// (required by ValidateConfig) and one serve-enabled site — rather than
// yaml.Marshal-ing a config.FileConfig, since config.Duration only
// implements UnmarshalYAML (it round-trips "15s" style strings on read,
// not on write).
func writeConfigYAML(t *testing.T, path string, hostServerPort int, siteName, domain string, listenPort int, backend string, addons []string) {
	t.Helper()
	addonsYAML := ""
	for _, a := range addons {
		addonsYAML += fmt.Sprintf("      - %q\n", a)
	}
	content := fmt.Sprintf(`hosts:
  - name: dummy
    host_ip: 127.0.0.1
    host_port: 1
    server_port: %d
sites:
  - name: %s
    addons:
%s    serve:
      domain: %s
      listen_addr: "127.0.0.1:%d"
      backend: %q
      acme_email: test@example.com
      dns_provider: cloudflare
      cloudflare:
        api_token_env: TEST_EDGE_CF_TOKEN
`, hostServerPort, siteName, addonsYAML, domain, listenPort, backend)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dialTLSWithRetry(t *testing.T, addr, serverName string, timeout time.Duration) *tls.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
		if err == nil {
			return conn
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("could not dial %s within %s: %v", addr, timeout, lastErr)
	return nil
}

func TestEdgeReverseProxyAddonRoundTrip(t *testing.T) {
	t.Setenv("TEST_EDGE_CF_TOKEN", "unused-in-test")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-From-Addon") != "1" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("missing addon header"))
			return
		}
		w.Write([]byte("hello SECRET world"))
	}))
	defer backend.Close()

	dir := t.TempDir()
	addonsDir := filepath.Join(dir, "addons")
	writeScript(t, addonsDir, "mod.star", `
def request(flow):
    flow.request.headers["X-From-Addon"] = "1"

def response(flow):
    flow.response.text = flow.response.text.replace("SECRET", "REDACTED")
    flow.response.headers["X-Modified"] = "yes"
`)

	configPath := filepath.Join(dir, "config.yaml")
	sitePort := freePort(t)
	writeConfigYAML(t, configPath, freePort(t), "netflix", "test.example", sitePort, backend.URL, []string{"mod.star"})

	store, err := config.LoadInitial(configPath)
	if err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}

	m := edge.NewManagerForTesting(store, addon.NewRuntime(), fakeCertFunc())
	m.Reconcile()

	conn := dialTLSWithRetry(t, fmt.Sprintf("127.0.0.1:%d", sitePort), "test.example", 3*time.Second)
	defer conn.Close()

	if got := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; got != "test.example" {
		t.Fatalf("server presented cert for CN=%q, want test.example", got)
	}

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test.example\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
	if string(body) != "hello REDACTED world" {
		t.Fatalf("body = %q, want addon's response-hook rewrite applied", body)
	}
	if resp.Header.Get("X-Modified") != "yes" {
		t.Fatal("missing X-Modified header from response hook")
	}
}

func TestEdgeReverseProxyJoinsBackendPathAndQuery(t *testing.T) {
	t.Setenv("TEST_EDGE_CF_TOKEN", "unused-in-test")

	var gotPath, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	sitePort := freePort(t)
	writeConfigYAML(t, configPath, freePort(t), "netflix", "test.example", sitePort, backend.URL+"/api?shared=1", nil)

	store, err := config.LoadInitial(configPath)
	if err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}

	m := edge.NewManagerForTesting(store, addon.NewRuntime(), fakeCertFunc())
	m.Reconcile()

	conn := dialTLSWithRetry(t, fmt.Sprintf("127.0.0.1:%d", sitePort), "test.example", 3*time.Second)
	defer conn.Close()

	fmt.Fprintf(conn, "GET /users?id=5 HTTP/1.1\r\nHost: test.example\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if gotPath != "/api/users" {
		t.Fatalf("backend saw path %q, want /api/users (serve.backend's own path prefixed)", gotPath)
	}
	if gotQuery != "shared=1&id=5" {
		t.Fatalf("backend saw query %q, want shared=1&id=5 (serve.backend's own query merged in)", gotQuery)
	}
}

func TestEdgeReverseProxyErrorHandlerDoesNotLeakBackendDetails(t *testing.T) {
	t.Setenv("TEST_EDGE_CF_TOKEN", "unused-in-test")

	unreachablePort := freePort(t) // nothing listens here
	backendURL := fmt.Sprintf("http://127.0.0.1:%d", unreachablePort)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	sitePort := freePort(t)
	writeConfigYAML(t, configPath, freePort(t), "netflix", "test.example", sitePort, backendURL, nil)

	store, err := config.LoadInitial(configPath)
	if err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}

	m := edge.NewManagerForTesting(store, addon.NewRuntime(), fakeCertFunc())
	m.Reconcile()

	conn := dialTLSWithRetry(t, fmt.Sprintf("127.0.0.1:%d", sitePort), "test.example", 3*time.Second)
	defer conn.Close()

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test.example\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(string(body), fmt.Sprintf("%d", unreachablePort)) {
		t.Fatalf("client-visible error body %q leaks the backend's port", body)
	}
	if strings.Contains(string(body), "127.0.0.1") {
		t.Fatalf("client-visible error body %q leaks the backend's address", body)
	}
}

func TestEdgeManagerReconcileRestartsOnBackendChange(t *testing.T) {
	t.Setenv("TEST_EDGE_CF_TOKEN", "unused-in-test")

	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-a"))
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-b"))
	}))
	defer backendB.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	sitePort := freePort(t)
	writeConfigYAML(t, configPath, freePort(t), "netflix", "test.example", sitePort, backendA.URL, nil)

	store, err := config.LoadInitial(configPath)
	if err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}

	m := edge.NewManagerForTesting(store, addon.NewRuntime(), fakeCertFunc())
	m.Reconcile()

	get := func() string {
		conn := dialTLSWithRetry(t, fmt.Sprintf("127.0.0.1:%d", sitePort), "test.example", 3*time.Second)
		defer conn.Close()
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test.example\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	if got := get(); got != "from-a" {
		t.Fatalf("body = %q, want from-a", got)
	}

	// Same listen_addr and domain, only the backend changes — a hot reload
	// must still pick this up.
	future := time.Now().Add(time.Hour)
	writeConfigYAML(t, configPath, freePort(t), "netflix", "test.example", sitePort, backendB.URL, nil)
	os.Chtimes(configPath, future, future)
	store.ReloadIfChanged()
	m.Reconcile()

	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = get()
		if last == "from-b" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last != "from-b" {
		t.Fatalf("body = %q, want from-b after backend-only config change picked up on reload", last)
	}
}

func TestEdgeManagerReconcileStartStopRebind(t *testing.T) {
	t.Setenv("TEST_EDGE_CF_TOKEN", "unused-in-test")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	port1 := freePort(t)
	writeConfigYAML(t, configPath, freePort(t), "netflix", "test.example", port1, backend.URL, nil)

	store, err := config.LoadInitial(configPath)
	if err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}

	m := edge.NewManagerForTesting(store, addon.NewRuntime(), fakeCertFunc())
	m.Reconcile()

	conn := dialTLSWithRetry(t, fmt.Sprintf("127.0.0.1:%d", port1), "test.example", 3*time.Second)
	conn.Close()

	// Rebind to a new port.
	port2 := freePort(t)
	future := time.Now().Add(time.Hour)
	writeConfigYAML(t, configPath, freePort(t), "netflix", "test.example", port2, backend.URL, nil)
	os.Chtimes(configPath, future, future)
	store.ReloadIfChanged()
	m.Reconcile()

	dialTLSWithRetry(t, fmt.Sprintf("127.0.0.1:%d", port2), "test.example", 3*time.Second).Close()

	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port1), 200*time.Millisecond); err == nil {
		t.Fatal("old listen_addr should have been stopped after rebind")
	}

	// Remove the site entirely.
	future2 := time.Now().Add(2 * time.Hour)
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("hosts:\n  - name: dummy\n    host_ip: 127.0.0.1\n    host_port: 1\n    server_port: %d\n", freePort(t))), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(configPath, future2, future2)
	store.ReloadIfChanged()
	m.Reconcile()

	time.Sleep(100 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port2), 200*time.Millisecond); err == nil {
		t.Fatal("site's listener should have stopped after removal from config")
	}
}
