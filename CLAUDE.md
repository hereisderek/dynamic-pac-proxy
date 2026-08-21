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

`main.go` (package `main`, at the repo root) is a thin entrypoint — flag
parsing, wiring, the top-level HTTP mux — that delegates all real logic to
packages under `internal/`. It runs on a homelab box and turns a fixed
local address into a forward proxy that chains to Charles (or falls back
to `DIRECT`) depending on whether Charles is currently reachable — see
"Why the architecture looks like this" below for why it's shaped this way.

Tests live outside the packages they test, under `tests/<package>/`, as
external (`_test` suffixed) test packages exercising only the exported API
— e.g. `tests/proxy/forwardproxy_test.go` is `package proxy_test` importing
`internal/proxy`. This is why the `internal/` packages export things like
`config.Store`, `health.State.SetSnapshot`, and `webui.ListCertFiles` that
a single-package `main` binary wouldn't otherwise need to: splitting into
packages forced a real internal API, which is also what makes black-box
testing from a separate directory possible in Go (a `_test.go` file can
only see another package's *unexported* identifiers if it physically lives
in that package's own directory — see the Go tour on external test
packages).

- **`main.go`** — flag parsing (`--config`, `--install`, `--uninstall`),
  embeds `deploy/config.yaml` (`go:embed`) and passes it down to
  `internal/install`, wires up `proxy.Manager` + a fixed 3s config-file
  poll (`configPollInterval`), and the HTTP mux serving
  `/proxy/<name>.pac`, `/status`, and `/certs`. There is no combined
  `/proxy.pac` route — removed on purpose, see below. The `go:embed`
  directive is why `deploy/config.yaml` stays at the repo root instead of
  moving under `internal/install/`: embed patterns can't contain `..`, so
  whichever package embeds it has to be an ancestor of `deploy/` — the
  root is the only package that is.
- **`internal/config`** — YAML schema (`FileConfig`/`HostConfig`), a custom
  `Duration` type so YAML holds `"15s"` strings instead of raw nanoseconds,
  `EffectiveHost` (per-host overrides merged onto global defaults), and
  `ValidateConfig` (unique host names, host_port/server_port ranges,
  `server_port` collisions against other hosts and against `listen_addr`).
  `Store` hot-reloads on mtime change but keeps the last known-good config
  if a reload fails to parse/validate — a bad edit is logged, never fatal,
  once the service is already running. Also owns `ResolveConfigPath` (flag
  → `/etc/dynamic-pac-proxy/config.yaml` → `config.yaml` beside the binary
  → fallback default), `ResolveCertsDir`/`Store.CertsDir` (the directory
  served at `/certs`, resolved fresh on every request so a hot-reloaded
  `certs_dir` takes effect without a restart), and `Store.ConfigDir` (the
  directory the config file itself lives in — used to place the local CA's
  private key somewhere that, unlike `CertsDir`, is never served). Per-host
  `HostConfig.InterceptSSL` (`intercept_ssl` in YAML) is read straight off
  the live config on every CONNECT rather than merged into
  `EffectiveHost`, since it has no top-level default to inherit — see
  `internal/proxy`. Each host identifies its target with exactly one of
  `HostName` or `HostIP` (`ValidateConfig` rejects both-set and
  neither-set); `HostConfig.Target()` returns whichever is set, for
  display (logs, `/status`) — see `internal/health` for where the actual
  branch (resolve vs. skip straight to dialing) happens.
- **`internal/health`** — a **lazy, TTL-gated** reachability cache.
  `State.GetFresh()` only does a real resolve (mDNS, or none at all when
  `EffectiveHost.HostIP` is set — `checkHost()` skips straight to
  `net.ParseIP` and the TCP dial) + TCP dial when the cached `Snapshot` is
  older than that host's `refresh_interval`; concurrent callers on a stale
  cache are coalesced onto one check via `checkMu`, not one each. There is
  deliberately no background polling ticker per host. `State.ReportFailure()` is the other
  way a check gets triggered early: when a request actually fails on the
  chained path, it invalidates the cache (zeroes `LastCheck`) so the next
  request re-checks immediately instead of waiting out the rest of
  `refresh_interval` — rate-limited by `failure_cooldown` so a burst of
  failing requests forces one recheck, not one per request.
  `State.SetSnapshot()` exists to seed/override the cache without a real
  network check — used by tests.
- **`internal/proxy`** — the actual proxying, plus listener lifecycle:
  - `NewHostHandler` builds the per-host forward-proxy handler. The
    chain-or-direct decision is made once per request (not inside
    `Transport.Proxy`) and threaded through the request context as an
    `upstreamDecision`, specifically so `ErrorHandler` can tell whether a
    failure happened on the chained path (→ call `reportUpstreamFailure`)
    or the direct path (→ leave the cache alone, since that failure has
    nothing to do with Charles). The plain-HTTP and (decrypted)
    intercepted-HTTPS paths share one handler — `newProxyHandler` — since
    once a request has a scheme+host filled in, "decide chained-vs-DIRECT,
    log, forward" is identical either way.
  - `handleConnect` dispatches a CONNECT to one of two implementations
    depending on that host's `intercept_ssl`:
    - `handleConnectTunnel` (default): hijacks the client connection and
      splices it byte-for-byte to an upstream tunnel — chained through
      Charles via our own CONNECT (`chainedConnect`) if reachable, or
      dialed straight to the target otherwise; a failed chained attempt
      calls `reportUpstreamFailure` before falling back. TLS terminates at
      the client and the real destination, unmodified — this box never
      sees plaintext.
    - `handleConnectIntercept` (`intercept_ssl: true`): hijacks the client
      connection, then TLS-terminates it itself using a certificate the
      shared local CA (`internal/mitm`) issues on the fly for the SNI it
      sees, and serves the decrypted HTTP/1.1 traffic through
      `newProxyHandler` — via `singleConnListener`, a one-shot
      `net.Listener` adapter that lets `http.Server` drive request/response
      parsing (keep-alive included) over the already-terminated connection
      instead of a hand-rolled read loop. `handleConnect` falls back to
      `handleConnectTunnel` if the CA can't be loaded, rather than breaking
      the connection outright.
  - `Manager` owns one `net.Listener` + `http.Server` per configured host;
    `Reconcile()` starts/stops/rebinds them as the config changes. It also
    lazily loads/generates the one shared `mitm.CA` (`getCA`, `sync.Once`)
    the moment any host with `intercept_ssl` actually needs it — nothing
    is touched on disk before then, same as the health cache never
    checking until traffic asks it to.
  - Every request (plain HTTP or decrypted-HTTPS alike) is logged with the
    client's own address (`clientIP`, i.e. `r.RemoteAddr`) alongside the
    target and chained/DIRECT outcome — devices talk straight to this box,
    so that's always the real originating device, never Charles's or the
    target's. Under `intercept_ssl` this logs the real decrypted URL, not
    just the CONNECT `host:port`.
- **`internal/mdns`** — `ResolveA` is a from-scratch mDNS (RFC 6762)
  A-record resolver over raw multicast UDP
  (`golang.org/x/net/dns/dnsmessage`), not a shell-out to `avahi-resolve`
  — deliberate, so it works on a minimal container with no `avahi-daemon`
  running.
- **`internal/mitm`** — dynamic-pac-proxy's own certificate authority,
  used only by hosts with `intercept_ssl` enabled. `LoadOrCreate` loads an
  existing CA from disk or generates a fresh ECDSA P-256 one (Common Name
  "Dynamic PAC Proxy Local CA") if missing/expired; the public cert is
  meant to be written under the certs directory (so it surfaces on
  `/certs` automatically) while the private key must be written somewhere
  `internal/webui` never serves — `internal/proxy.Manager.getCA` is what
  actually picks those two paths (certs dir vs. `config.Store.ConfigDir`)
  before calling this. `CA.LeafCertificate`/`CertificateFor` issue and
  cache (by hostname) a fresh leaf certificate signed by that CA per SNI —
  this is what lets `handleConnectIntercept` present a trusted-once-the-CA-
  is-installed certificate for whatever domain the client is asking for.
- **`internal/webui`** — the auxiliary HTTP endpoints, as opposed to the
  per-host proxy ports in `internal/proxy`:
  - `index.go`: `IndexHandler` serves `/` — one entry per host with its
    PAC URL and manual `advertise_host:server_port` address, built from
    `config.PortFromAddr(cfg.ListenAddr)` plus `HostConfig.Target()`. It
    only matches the exact root path itself and 404s otherwise, since
    `http.ServeMux` treats a registered `"/"` as a catch-all for every
    unmatched path — without that check, a typo'd URL would silently
    render this page instead of 404ing.
  - `pac.go`: `BuildPAC` is static: always
    `PROXY advertise_host:server_port; DIRECT`. `WriteStatusJSON` also
    forces a fresh health check per host when called — hitting `/status`
    counts as "a request came in" for the lazy-check design. `HostStatus`'s
    JSON fields (`host_port`/`server_port`) intentionally mirror the
    config's own field names — nothing outside this repo depends on the
    older `port`/`listen_port` names (checked against the openwrt LuCI
    app's live-status JS, which only reads `name`/`hostname`/
    `resolved_ip`/`reachable`/`last_check`/`error`).
  - `certs.go`: `CertsIndexHandler`/`CertsFileHandler` serve `/certs` — a
    page listing whatever certificate files are in the config's certs
    directory (see `internal/config.ResolveCertsDir`), with per-platform
    install instructions, plus the actual file downloads with a
    content-type mobile OSes recognize as an installable cert/profile
    (`certMimeTypes`). Charles's own certificate is never bundled with the
    binary — Charles generates its own root CA per install; see
    `certs/README.md` for how a user exports and drops theirs in.
    dynamic-pac-proxy's own generated CA cert (`internal/mitm`) lands in
    the same directory under its own filename, so it's listed here too,
    automatically, the moment any host's `intercept_ssl` first needs it.
- **`internal/install`** — `Run`/`Uninstall` (called from `main.go` for
  `--install`/`--uninstall`): detects the init system (`systemd` via
  `/run/systemd/system`, OpenRC via `rc-service` on `PATH`), renders +
  writes the matching service definition pointed at the *actual* running
  binary's path and the resolved config path, and seeds a config file from
  the seed YAML `main.go` hands it (embedded from `deploy/config.yaml`) if
  none exists yet. It does not (yet) recognize OpenWRT's `procd` — that's
  set up manually, see README, or via the `openwrt/` package below.
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
  decision on every request via `internal/proxy`. Don't move that
  decision back into the PAC.
- **Each host gets its own dedicated `server_port`**, not a shared port.
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
- **`host_ip` exists as an escape hatch, not a replacement for mDNS**:
  added for hosts that either already have a fixed address (e.g. a
  mitmproxy instance) or can't answer mDNS at all — and it happens to also
  sidestep the "Important networking caveat" in README.md (multicast not
  crossing an LXC's NAT/routed subnet) for a specific host, without
  needing an mDNS reflector on the whole network. It's deliberately
  exactly one extra field, not a second resolution mechanism living
  alongside `host_name` with its own timeout/retry semantics —
  `checkHost()` just skips resolution and dials the parsed IP directly.

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
  `server_port: 8081`.
- **Pending migration**: the box's live `/opt/dynamic-pac-proxy/config.yaml`
  (as of the last deploy) still uses the pre-rename field names
  (`mdns_hostname`/`port`/`listen_port`). A newer binary parses those as
  unknown keys — `host_name`/`host_ip` both end up empty, which
  `ValidateConfig` rejects, so the service **fails to start** on the next
  restart until that file is rewritten with `host_name`/`host_port`/
  `server_port`. Rewrite it (by hand or `scp`'d from an updated
  `deploy/config.yaml`) in the same deploy that ships the new binary —
  don't just swap the binary alone.
