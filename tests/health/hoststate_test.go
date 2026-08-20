package health_test

import (
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
		MDNSHostname:    "definitely-does-not-exist.local",
		Port:            9999,
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
