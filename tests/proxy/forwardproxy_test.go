package proxy_test

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/health"
	"github.com/derekhud/dynamic-pac-proxy/internal/proxy"
)

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
			{Name: "test-host", MDNSHostname: "unused.local", Port: charlesPort, ListenPort: 0},
		},
	}, "")

	states := health.NewStateStore()
	handler := proxy.NewHostHandler(cfgStore, states, "test-host")
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
			IP: net.ParseIP(charlesIP), Port: charlesPort, Reachable: true, LastCheck: time.Now(),
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
			IP: net.ParseIP(charlesIP), Port: charlesPort, Reachable: true, LastCheck: time.Now(),
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
			IP: net.ParseIP("127.0.0.1"), Port: deadPort, Reachable: true, LastCheck: time.Now(),
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
			IP: net.ParseIP("127.0.0.1"), Port: deadPort, Reachable: true, LastCheck: time.Now(),
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
