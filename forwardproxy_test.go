package main

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
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

func setHostSnapshot(states *stateStore, name string, snap hostSnapshot) {
	hs := states.get(name)
	hs.mu.Lock()
	hs.snap = snap
	hs.mu.Unlock()
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

	cfgStore := &configStore{}
	cfgStore.cfg = fileConfig{
		ListenAddr:      ":0",
		RefreshInterval: duration(time.Hour), // long TTL: our manual snapshot below must stick
		MDNSTimeout:     duration(time.Second),
		DialTimeout:     duration(2 * time.Second),
		FailureCooldown: duration(5 * time.Second),
		Hosts: []hostConfig{
			{Name: "test-host", MDNSHostname: "unused.local", Port: charlesPort, ListenPort: 0},
		},
	}

	states := newStateStore()
	handler := newHostProxyHandler(cfgStore, states, "test-host")
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
		setHostSnapshot(states, "test-host", hostSnapshot{
			ip: net.ParseIP(charlesIP), port: charlesPort, reachable: true, lastCheck: time.Now(),
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
		setHostSnapshot(states, "test-host", hostSnapshot{
			ip: net.ParseIP(charlesIP), port: charlesPort, reachable: true, lastCheck: time.Now(),
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
		setHostSnapshot(states, "test-host", hostSnapshot{reachable: false, lastCheck: time.Now()})
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
		setHostSnapshot(states, "test-host", hostSnapshot{reachable: false, lastCheck: time.Now()})
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
		states.remove("test-host")
		deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
		deadPort := deadLn.Addr().(*net.TCPAddr).Port
		deadLn.Close()

		setHostSnapshot(states, "test-host", hostSnapshot{
			ip: net.ParseIP("127.0.0.1"), port: deadPort, reachable: true, lastCheck: time.Now(),
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

		if !states.get("test-host").peek().lastCheck.IsZero() {
			t.Fatal("expected the failed chained CONNECT to invalidate the cache instead of waiting out refresh_interval")
		}
	})

	t.Run("chained plain HTTP fails and invalidates the cache (no same-request fallback)", func(t *testing.T) {
		states.remove("test-host") // fresh cooldown, same reason as above
		deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
		deadPort := deadLn.Addr().(*net.TCPAddr).Port
		deadLn.Close()

		setHostSnapshot(states, "test-host", hostSnapshot{
			ip: net.ParseIP("127.0.0.1"), port: deadPort, reachable: true, lastCheck: time.Now(),
		})
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("expected 502 for a failed chained attempt, got %d", resp.StatusCode)
		}

		if !states.get("test-host").peek().lastCheck.IsZero() {
			t.Fatal("expected the failed chained plain HTTP request to invalidate the cache instead of waiting out refresh_interval")
		}
	})

	t.Run("DIRECT failures don't invalidate the cache", func(t *testing.T) {
		// The destination itself being unreachable is unrelated to Charles's
		// health and must not be mistaken for a chained-path failure.
		states.remove("test-host")
		setHostSnapshot(states, "test-host", hostSnapshot{reachable: false, lastCheck: time.Now()})

		deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
		deadTarget := deadLn.Addr().String()
		deadLn.Close()

		req, _ := http.NewRequest(http.MethodGet, "http://"+deadTarget, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}

		snap := states.get("test-host").peek()
		if snap.lastCheck.IsZero() || snap.reachable {
			t.Fatalf("a DIRECT-path failure should not touch the cached reachable=false state, got %+v", snap)
		}
	})
}
