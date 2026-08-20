# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go mod tidy                                    # fetch deps, update go.sum
go build -o dynamic-pac-proxy .                # build for the current OS/arch
GOOS=linux GOARCH=amd64 go build -o dynamic-pac-proxy-linux-amd64 .   # cross-compile for the LXC target
go vet ./...
go test ./...                                  # full suite
go test -run TestForwardProxyChainedAndDirect -v ./...   # single test
go test -run TestForwardProxyChainedAndDirect/'chained_CONNECT.*' -v ./...   # single subtest
```

There is no linter config beyond `go vet`.

## Architecture

This is a single Go binary (package `main`, no internal packages) that runs
on a homelab box and turns a fixed local address into a forward proxy that
chains to Charles (or falls back to `DIRECT`) depending on whether Charles
is currently reachable — see "Why the architecture looks like this" below
for why it's shaped this way.

- **`main.go`** — flag parsing (`--config`, `--install`, `--uninstall`),
  `resolveConfigPath()` (explicit flag → `/etc/dynamic-pac-proxy/config.yaml`
  → `config.yaml` beside the binary → fallback default), wires up the
  `hostManager` + a fixed 3s config-file poll (`configPollInterval`), and
  the HTTP mux serving `/proxy/<name>.pac` and `/status`. There is no
  combined `/proxy.pac` route — removed on purpose, see below.
- **`config.go`** — YAML schema (`fileConfig`/`hostConfig`), a custom
  `duration` type so YAML holds `"15s"` strings instead of raw nanoseconds,
  `effectiveHost` (per-host overrides merged onto global defaults), and
  `validateConfig` (unique host names, port/listen_port ranges,
  `listen_port` collisions against other hosts and against `listen_addr`).
  `configStore` hot-reloads on mtime change but keeps the last known-good
  config if a reload fails to parse/validate — a bad edit is logged, never
  fatal, once the service is already running.
- **`hoststate.go`** — two things:
  - `hostState`/`stateStore`: a **lazy, TTL-gated** health cache.
    `getFresh()` only does a real mDNS resolve + TCP dial
    (`checkHost()`) when the cached result is older than that host's
    `refresh_interval`; concurrent callers on a stale cache are coalesced
    onto one check via `checkMu`, not one each. There is deliberately no
    background polling ticker per host. `reportFailure()` is the other way
    a check gets triggered early: when a request actually fails on the
    chained path, it invalidates the cache (zeroes `lastCheck`) so the next
    request re-checks immediately instead of waiting out the rest of
    `refresh_interval` — rate-limited by `failure_cooldown` so a burst of
    failing requests forces one recheck, not one per request.
  - `hostManager`: owns one `net.Listener` + `http.Server` per configured
    host; `reconcile()` starts/stops/rebinds them as the config changes.
- **`forwardproxy.go`** — the actual proxying. The chain-or-direct decision
  is made once per request (not inside `Transport.Proxy`) and threaded
  through the request context as an `upstreamDecision`, specifically so
  `ErrorHandler` can tell whether a failure happened on the chained path
  (→ call `reportFailure`) or the direct path (→ leave the cache alone,
  since that failure has nothing to do with Charles). `handleConnect`
  implements HTTPS tunneling by hijacking the client connection and
  splicing it to an upstream tunnel — chained through Charles via our own
  CONNECT (`chainedConnect`) if reachable, or dialed straight to the target
  otherwise; a failed chained attempt calls `reportUpstreamFailure` before
  falling back.
- **`mdns.go`** — `ResolveA` is a from-scratch mDNS (RFC 6762) A-record
  resolver over raw multicast UDP (`golang.org/x/net/dns/dnsmessage`), not
  a shell-out to `avahi-resolve` — deliberate, so it works on a minimal
  container with no `avahi-daemon` running.
- **`pac.go`** — `buildPAC` is now static: always
  `PROXY advertise_host:listen_port; DIRECT`. `writeStatusJSON` also
  forces a fresh health check per host when called — hitting `/status`
  counts as "a request came in" for the lazy-check design.
- **`install.go`** — `--install`/`--uninstall`: detects the init system
  (`systemd` via `/run/systemd/system`, OpenRC via `rc-service` on
  `PATH`), renders + writes the matching service definition pointed at the
  *actual* running binary's path and the resolved config path, and seeds a
  config file from the embedded `deploy/config.yaml` (`go:embed`) if none
  exists yet. It does not (yet) recognize OpenWRT's `procd` — that's set
  up manually, see README, or via the `openwrt/` package below.
- **`openwrt/`** — a self-contained OpenWRT package (UCI config + LuCI app
  + `.ipk`/SSH installer scripts), deliberately kept out of the main Go
  tree. It never touches this daemon's code: a shell script
  (`uci2yaml.sh`) translates `/etc/config/dynamic-pac-proxy` (UCI) into
  the same `config.yaml` the daemon already reads, leaning on the
  existing hot-reload so most LuCI edits apply live without a restart.
  Not yet tested against a real router/LuCI — see `openwrt/README.md`'s
  "What's verified, what isn't" section before assuming it's fully
  working.

### Why the architecture looks like this

The design went through a real pivot mid-project, and future changes
should not accidentally regress it:

- **PAC used to return Charles's resolved, moving IP directly** (with a
  `; DIRECT` fallback). This was abandoned because client devices didn't
  reliably re-fetch the PAC file when Charles's reachability changed, so
  they'd get stuck on a stale answer. The fix: PAC now returns this box's
  own **fixed** address, and this box does the real-time reachability
  decision on every request via `forwardproxy.go`. Don't move that
  decision back into the PAC.
- **Each host gets its own dedicated `listen_port`**, not a shared port.
  There used to be a combined `/proxy.pac` ("first reachable host in
  config order wins"); it was removed when per-host dedicated ports were
  chosen, because at that point it no longer had a coherent meaning (each
  port already handles its own failover internally).
- **Health checks are lazy/TTL-cached, not a background poll loop** —
  changed specifically for efficiency: don't add back a ticker-per-host
  goroutine that runs regardless of traffic.
- **mDNS self-query quirk**: resolving a `.local` hostname from the same
  machine that owns it will not get a real wire response (macOS's
  `mDNSResponder` loopback-shortcuts self-queries — `ping` to your own
  hostname resolves via `127.0.0.1` without ever touching the network).
  This is a testing artifact when developing on the same Mac that's being
  resolved, not a bug in `ResolveA`.

## Current deployment

- Target box: Alpine Linux (OpenRC) LXC container on Proxmox, x86_64, at
  `172.16.2.22`. Binary + config live side by side in
  `/opt/dynamic-pac-proxy/` (`dynamic-pac-proxy`, `config.yaml`), managed
  by the generated OpenRC script at `/etc/init.d/dynamic-pac-proxy`
  (`supervise-daemon`), logging to `/var/log/dynamic-pac-proxy.log`.
- Redeploy pattern used so far: cross-compile locally, `scp` the binary to
  a `.new` file on the box, `mv` it into place (avoids clobbering a
  binary a running process might still be reading mid-copy), then
  `rc-service dynamic-pac-proxy restart`.
- The proxied machine: a Mac at `dereks-MacBook-Pro.local` running Charles
  on port `8888`, configured in `hosts:` as `derek-macbook` with
  `listen_port: 8081`.
