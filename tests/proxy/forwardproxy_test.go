package proxy_test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/health"
	"github.com/derekhud/dynamic-pac-proxy/internal/mitm"
	"github.com/derekhud/dynamic-pac-proxy/internal/proxy"
)

// noCA is a caGetter for tests that don't exercise intercept_ssl — it's
// never actually called unless a host has intercept_ssl enabled.
func noCA() (*mitm.CA, error) {
	return nil, fmt.Errorf("CA not needed for this test")
}

// startFakeCharles is a minimal stand-in for Charles: it handles CONNECT by
// tunneling bytes, and plain absolute-URI GET by fetching and relaying —
// just enough to exercise our "chain through upstream" code path in a test.
func startFakeCharles(t *testing.T) (ip string, port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			target, err := net.DialTimeout("tcp", r.Host, 2*time.Second)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			hj := w.(http.Hijacker)
			client, _, _ := hj.Hijack()
			client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() { io.Copy(target, client); target.Close() }()
			io.Copy(client, target)
			client.Close()
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})}
	go srv.Serve(ln)
	tcpAddr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", tcpAddr.Port, func() { srv.Close() }
}

func TestForwardProxyChainedAndDirect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from plain origin"))
	}))
	defer origin.Close()

	tlsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from tls origin"))
	}))
	defer tlsOrigin.Close()

	charlesIP, charlesPort, stopCharles := startFakeCharles(t)
	defer stopCharles()

	cfgStore := config.NewStore(config.FileConfig{
		ListenAddr:      ":0",
		RefreshInterval: config.Duration(time.Hour), // long TTL: our manual snapshot below must stick
		MDNSTimeout:     config.Duration(time.Second),
		DialTimeout:     config.Duration(2 * time.Second),
		FailureCooldown: config.Duration(5 * time.Second),
		Hosts: []config.HostConfig{
			{Name: "test-host", HostName: "unused.local", HostPort: charlesPort, ServerPort: 0},
		},
	}, "")

	states := health.NewStateStore()
	handler := proxy.NewHostHandler(cfgStore, states, "test-host", noCA)
	proxySrv := httptest.NewServer(handler)
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			// Each subtest changes the health cache and expects its request
			// to actually re-enter our handler; a pooled/reused CONNECT
			// tunnel from an earlier subtest would silently skip it.
			DisableKeepAlives: true,
		},
	}

	t.Run("chained plain HTTP via fake Charles", func(t *testing.T) {
		states.Get("test-host").SetSnapshot(health.Snapshot{
			IP: net.ParseIP(charlesIP), HostPort: charlesPort, Reachable: true, LastCheck: time.Now(),
		})
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "hello from plain origin" {
			t.Fatalf("unexpected body: %q", body)
		}
	})

	t.Run("chained CONNECT (HTTPS) via fake Charles", func(t *testing.T) {
		states.Get("test-host").SetSnapshot(health.Snapshot{
			IP: net.ParseIP(charlesIP), HostPort: charlesPort, Reachable: true, LastCheck: time.Now(),
		})
		resp, err := client.Get(tlsOrigin.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "hello from tls origin" {
			t.Fatalf("unexpected body: %q", body)
		}
	})

	t.Run("DIRECT plain HTTP when host unreachable", func(t *testing.T) {
		states.Get("test-host").SetSnapshot(health.Snapshot{Reachable: false, LastCheck: time.Now()})
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "hello from plain origin" {
			t.Fatalf("unexpected body: %q", body)
		}
	})

	t.Run("DIRECT CONNECT (HTTPS) when host unreachable", func(t *testing.T) {
		states.Get("test-host").SetSnapshot(health.Snapshot{Reachable: false, LastCheck: time.Now()})
		resp, err := client.Get(tlsOrigin.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "hello from tls origin" {
			t.Fatalf("unexpected body: %q", body)
		}
	})

	t.Run("chained CONNECT falls back to DIRECT when Charles refuses, and invalidates the cache", func(t *testing.T) {
		// Point at a closed port so the chained CONNECT attempt itself fails,
		// exercising the fallback-to-DIRECT branch inside handleConnect.
		// Fresh host state so an earlier subtest's failure cooldown can't
		// mask this one's invalidation.
		states.Remove("test-host")
		deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
		deadPort := deadLn.Addr().(*net.TCPAddr).Port
		deadLn.Close()

		states.Get("test-host").SetSnapshot(health.Snapshot{
			IP: net.ParseIP("127.0.0.1"), HostPort: deadPort, Reachable: true, LastCheck: time.Now(),
		})
		resp, err := client.Get(tlsOrigin.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "hello from tls origin" {
			t.Fatalf("unexpected body: %q", body)
		}

		if !states.Get("test-host").Peek().LastCheck.IsZero() {
			t.Fatal("expected the failed chained CONNECT to invalidate the cache instead of waiting out refresh_interval")
		}
	})

	t.Run("chained plain HTTP fails and invalidates the cache (no same-request fallback)", func(t *testing.T) {
		states.Remove("test-host") // fresh cooldown, same reason as above
		deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
		deadPort := deadLn.Addr().(*net.TCPAddr).Port
		deadLn.Close()

		states.Get("test-host").SetSnapshot(health.Snapshot{
			IP: net.ParseIP("127.0.0.1"), HostPort: deadPort, Reachable: true, LastCheck: time.Now(),
		})
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("expected 502 for a failed chained attempt, got %d", resp.StatusCode)
		}

		if !states.Get("test-host").Peek().LastCheck.IsZero() {
			t.Fatal("expected the failed chained plain HTTP request to invalidate the cache instead of waiting out refresh_interval")
		}
	})

	t.Run("DIRECT failures don't invalidate the cache", func(t *testing.T) {
		// The destination itself being unreachable is unrelated to Charles's
		// health and must not be mistaken for a chained-path failure.
		states.Remove("test-host")
		states.Get("test-host").SetSnapshot(health.Snapshot{Reachable: false, LastCheck: time.Now()})

		deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
		deadTarget := deadLn.Addr().String()
		deadLn.Close()

		req, _ := http.NewRequest(http.MethodGet, "http://"+deadTarget, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}

		snap := states.Get("test-host").Peek()
		if snap.LastCheck.IsZero() || snap.Reachable {
			t.Fatalf("a DIRECT-path failure should not touch the cached reachable=false state, got %+v", snap)
		}
	})
}

// TestInterceptSSL exercises intercept_ssl end to end up through the point
// this tool controls: the client completes a TLS handshake with a
// certificate the local CA issued on the fly for the requested SNI, and
// the decrypted request is correctly reconstructed as an absolute
// https://<host>/<path> URL and forwarded. The final hop (dialing the real
// "localhost:<port>" destination) is deliberately left with nothing
// listening, so the expected outcome is a 502 delivered back over the
// same TLS connection — that failure is real internet trust the tool
// doesn't control, not something worth faking a working origin for here.
func TestInterceptSSL(t *testing.T) {
	base := t.TempDir()
	certPath := filepath.Join(base, "ca.pem")
	keyPath := filepath.Join(base, "ca-key.pem")

	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("localhost:%d", deadLn.Addr().(*net.TCPAddr).Port)
	deadLn.Close()

	cfgStore := config.NewStore(config.FileConfig{
		ListenAddr:      ":0",
		RefreshInterval: config.Duration(time.Hour),
		MDNSTimeout:     config.Duration(time.Second),
		DialTimeout:     config.Duration(2 * time.Second),
		FailureCooldown: config.Duration(5 * time.Second),
		Hosts: []config.HostConfig{
			{Name: "intercept-host", HostName: "unused.local", HostPort: 1, ServerPort: 0, InterceptSSL: true},
		},
	}, filepath.Join(base, "config.yaml"))

	states := health.NewStateStore()
	// Unreachable, so the decrypted request goes DIRECT to `target` above.
	states.Get("intercept-host").SetSnapshot(health.Snapshot{Reachable: false, LastCheck: time.Now()})

	var ca *mitm.CA
	caGetter := func() (*mitm.CA, error) {
		var err error
		ca, err = mitm.LoadOrCreate(certPath, keyPath)
		return ca, err
	}

	handler := proxy.NewHostHandler(cfgStore, states, "intercept-host", caGetter)
	proxySrv := httptest.NewServer(handler)
	defer proxySrv.Close()

	rawConn, err := net.Dial("tcp", proxySrv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer rawConn.Close()

	fmt.Fprintf(rawConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	connectResp, err := http.ReadResponse(bufio.NewReader(rawConn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResp.StatusCode)
	}

	// caGetter runs synchronously inside CONNECT handling above (before the
	// 200 is written), so `ca` is populated by now — build a pool that
	// trusts it, exactly as a device would after installing
	// /certs/dynamic-pac-proxy-ca.pem.
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())

	tlsConn := tls.Client(rawConn, &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake with the intercepting proxy failed: %v", err)
	}

	peerCert := tlsConn.ConnectionState().PeerCertificates[0]
	if len(peerCert.DNSNames) != 1 || peerCert.DNSNames[0] != "localhost" {
		t.Fatalf("issued leaf certificate DNSNames = %v, want [localhost]", peerCert.DNSNames)
	}

	fmt.Fprintf(tlsConn, "GET /some/path HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target)
	httpResp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read decrypted response: %v", err)
	}
	if httpResp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (nothing is listening at %s)", httpResp.StatusCode, target)
	}
}
