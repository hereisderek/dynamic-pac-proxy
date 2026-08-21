package health_test

import (
	"net"
	"testing"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/config"
	"github.com/derekhud/dynamic-pac-proxy/internal/health"
)

// TestLazyHealthCheckTTL verifies the core efficiency contract: a health
// check only happens on a cache miss or after the TTL expires, never on
// every call.
func TestLazyHealthCheckTTL(t *testing.T) {
	eff := config.EffectiveHost{
		Name:            "ttl-host",
		HostName:        "definitely-does-not-exist.local",
		HostPort:        9999,
		RefreshInterval: 300 * time.Millisecond,
		MDNSTimeout:     150 * time.Millisecond,
		DialTimeout:     150 * time.Millisecond,
	}

	hs := health.NewStateStore().Get("ttl-host")

	start := time.Now()
	hs.GetFresh(eff)
	firstElapsed := time.Since(start)
	if firstElapsed < eff.MDNSTimeout {
		t.Fatalf("first call returned too fast (%s), expected a real mDNS attempt taking >= %s", firstElapsed, eff.MDNSTimeout)
	}

	start = time.Now()
	hs.GetFresh(eff)
	cachedElapsed := time.Since(start)
	if cachedElapsed > 20*time.Millisecond {
		t.Fatalf("second call within TTL took %s, expected an instant cache hit with no re-check", cachedElapsed)
	}

	time.Sleep(eff.RefreshInterval + 50*time.Millisecond)

	start = time.Now()
	hs.GetFresh(eff)
	staleElapsed := time.Since(start)
	if staleElapsed < eff.MDNSTimeout {
		t.Fatalf("call after TTL expiry returned too fast (%s), expected a fresh mDNS attempt taking >= %s", staleElapsed, eff.MDNSTimeout)
	}
}

// TestReportFailureCooldown verifies ReportFailure invalidates the cache
// immediately on the first call, but is a no-op (doesn't re-invalidate,
// doesn't reset the cooldown clock) for repeated calls within the cooldown
// window — only the first failure in a burst should force an early recheck.
func TestReportFailureCooldown(t *testing.T) {
	hs := health.NewStateStore().Get("cooldown-host")
	cooldown := 200 * time.Millisecond

	hs.SetSnapshot(health.Snapshot{Reachable: true, LastCheck: time.Now()})

	if ok := hs.ReportFailure(cooldown); !ok {
		t.Fatal("first ReportFailure should invalidate the cache")
	}
	if !hs.Peek().LastCheck.IsZero() {
		t.Fatal("expected LastCheck to be zeroed after invalidation")
	}

	if ok := hs.ReportFailure(cooldown); ok {
		t.Fatal("second ReportFailure within the cooldown window should be a no-op")
	}

	time.Sleep(cooldown + 50*time.Millisecond)

	hs.SetSnapshot(health.Snapshot{Reachable: true, LastCheck: time.Now()})

	if ok := hs.ReportFailure(cooldown); !ok {
		t.Fatal("ReportFailure after the cooldown window elapsed should invalidate again")
	}
}

// TestHostIPSkipsMDNS verifies a host configured with a fixed host_ip
// never goes through mDNS resolution at all — proven by using a
// deliberately long MDNSTimeout that a real check must NOT wait out.
func TestHostIPSkipsMDNS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listening: the dial itself should fail fast

	eff := config.EffectiveHost{
		Name:        "fixed-ip-host",
		HostIP:      "127.0.0.1",
		HostPort:    port,
		MDNSTimeout: 5 * time.Second, // would dominate elapsed time if (wrongly) used
		DialTimeout: time.Second,
	}

	hs := health.NewStateStore().Get("fixed-ip-host")

	start := time.Now()
	snap := hs.GetFresh(eff)
	elapsed := time.Since(start)

	if elapsed >= eff.MDNSTimeout {
		t.Fatalf("check took %s, expected it to skip mDNS entirely for a host_ip host (MDNSTimeout=%s)", elapsed, eff.MDNSTimeout)
	}
	if snap.Hostname != "127.0.0.1" {
		t.Fatalf("Snapshot.Hostname = %q, want the configured host_ip", snap.Hostname)
	}
	if snap.Reachable {
		t.Fatal("expected unreachable: nothing is listening on the dialed port")
	}
}
