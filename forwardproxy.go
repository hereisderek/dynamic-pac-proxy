package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"
)

// newHostProxyHandler builds the forward-proxy handler for one configured
// host. On every request it consults that host's lazily-refreshed health
// cache (see hoststate.go) and either chains through the upstream (Charles)
// if it's reachable, or goes DIRECT to the real destination if not.
func newHostProxyHandler(cfgStore *configStore, states *stateStore, hostName string) http.Handler {
	// currentUpstream resolves, for the request in flight, where traffic
	// should go: (proxyURL, true) to chain through it, (nil, true) to go
	// direct, or (nil, false) if the host was removed from the config.
	currentUpstream := func() (*url.URL, bool) {
		cfg := cfgStore.snapshot()
		h, ok := cfg.findHost(hostName)
		if !ok {
			return nil, false
		}
		eff := cfg.effective(h)
		snap := states.get(hostName).getFresh(eff)
		if !snap.reachable {
			return nil, true
		}
		return &url.URL{Scheme: "http", Host: net.JoinHostPort(snap.ip.String(), strconv.Itoa(snap.port))}, true
	}

	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
		Transport: &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				proxyURL, ok := currentUpstream()
				if !ok {
					return nil, fmt.Errorf("host %q is no longer configured", hostName)
				}
				return proxyURL, nil
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("host %q: proxy error for %s: %v", hostName, r.URL, err)
			http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r, cfgStore, states, hostName)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// handleConnect implements HTTPS tunneling: hijack the client connection,
// establish an upstream tunnel (chained through Charles if reachable, else
// straight to the target), and splice bytes between them.
func handleConnect(w http.ResponseWriter, r *http.Request, cfgStore *configStore, states *stateStore, hostName string) {
	cfg := cfgStore.snapshot()
	h, ok := cfg.findHost(hostName)
	if !ok {
		http.Error(w, "host no longer configured", http.StatusBadGateway)
		return
	}
	eff := cfg.effective(h)
	snap := states.get(hostName).getFresh(eff)

	var upstream net.Conn
	if snap.reachable {
		conn, err := chainedConnect(snap.ip.String(), snap.port, r.Host, eff.DialTimeout)
		if err != nil {
			log.Printf("host %q: chained CONNECT to %s via upstream failed, falling back to DIRECT: %v", hostName, r.Host, err)
		} else {
			upstream = conn
		}
	}
	if upstream == nil {
		conn, err := net.DialTimeout("tcp", r.Host, eff.DialTimeout)
		if err != nil {
			http.Error(w, "could not connect to "+r.Host+": "+err.Error(), http.StatusBadGateway)
			return
		}
		upstream = conn
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}

	go splice(client, upstream)
}

// chainedConnect dials the upstream proxy (Charles) and issues our own
// CONNECT on the client's behalf, returning the established tunnel.
func chainedConnect(upstreamHost string, upstreamPort int, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(upstreamHost, strconv.Itoa(upstreamPort)), timeout)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("upstream CONNECT returned %s", resp.Status)
	}
	// br may already hold bytes buffered past the response headers (the
	// start of the tunneled stream); read through it, not the raw conn.
	return &bufferedConn{Conn: conn, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func splice(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}
