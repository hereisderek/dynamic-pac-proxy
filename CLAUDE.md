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
  `internal/install`, wires up `proxy.Manager` + `edge.Manager` (sharing
  one `addon.Runtime` between them) + a fixed 3s config-file poll
  (`configPollInterval`) that `Reconcile()`s both, and the HTTP mux serving
  `/proxy/<name>.pac`, `/status`, and `/certs`. There is no combined
  `/proxy.pac` route — removed on purpose, see below. The `go:embed`
  directive is why `deploy/config.yaml` stays at the repo root instead of
  moving under `internal/install/`: embed patterns can't contain `..`, so
  whichever package embeds it has to be an ancestor of `deploy/` — the
  root is the only package that is.
- **`internal/config`** — YAML schema (`FileConfig`/`HostConfig`/
  `SiteConfig`/`SiteServe`), a custom
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
  branch (resolve vs. skip straight to dialing) happens. `SiteConfig`
  (`sites:`) is a named `Addons` bundle that currently only does anything
  via `Serve *SiteServe` — a standalone public reverse-proxy mirror; see
  `internal/edge`. `ValidateConfig` fails config load fast on a `serve`
  site's malformed domain/listen_addr/backend/acme_email, an unsupported
  `dns_provider`, or (deliberately, matching every other credential-shaped
  check here) a `cloudflare.api_token_env` that names an environment
  variable that isn't actually set — a missing secret should stop config
  load with a clear message now, not surface 60–90 days later as a
  silently failed certificate renewal. `CloudflareDNSConfig` accepts
  either `api_token_env` (preferred) or a literal `api_token` — exactly
  one, never both, enforced by `ValidateConfig`; `internal/edge`'s
  `buildDNSProvider` prefers the literal when both would somehow be
  present. `SiteServe.ACMEStaging` (`acme_staging` in YAML) is a plain
  bool with no validation of its own — routing issuance through Let's
  Encrypt's staging directory instead of production, for exercising the
  DNS-01/renewal pipeline without burning production's much stricter rate
  limits. `serve.listen_addr`'s port is checked against the same
  collision map as `hosts[].server_port` and `listen_addr`, since both
  are real listeners on this same process. Deliberately **not** validated
  here: whether `sites[].addons` script files exist or compile — see
  `internal/addon` for why that's a runtime concern instead.
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
- **`internal/addon`** — runs `sites[].addons` scripts against decrypted
  requests/responses, shared by `internal/edge` today and, in a future
  iteration, `internal/proxy`'s own intercept_ssl pipeline. Scripts are
  Starlark (`go.starlark.net` — a pure-Go, sandboxed, Python-syntax
  *subset*, not real Python or real mitmproxy compatibility, hence the
  `.star` extension rather than `.py`) with a mitmproxy-flavored shape
  (`def request(flow):`, `def response(flow):`) so a simple mitmproxy
  addon can be hand-ported. `Runtime.RunRequest`/`RunResponse` lazily
  compile-and-cache each script by absolute path + mtime (mirroring
  `config.Store`'s own mtime-gated reload, but per-script and triggered by
  traffic, not a poller), calling `globals.Freeze()` after compiling —
  required, not just tidy: `go.starlark.net`'s `StringDict` holds mutable
  values until frozen, and only a frozen script's cached function values
  are safe to call concurrently, each with its own fresh `*starlark.Thread`
  (`Thread`s themselves are never shared across goroutines). Freezing also
  turns a script that tries to keep mutable state across requests into a
  loud "cannot mutate frozen value" error instead of a silent race — addons
  are expected to be stateless across requests. `thread.SetMaxExecutionSteps`
  bounds a single hook call so a pathological script can't hang a
  request-handling goroutine; there's otherwise no step/time limit in
  Starlark itself. The `flow` object (`flow.go`) is a Go-backed
  `starlark.Value` implementing `HasSetField` so `flow.request.text = "..."`
  works with real assignment syntax; mutations build up on it and are only
  written back to the real `*http.Request`/`*http.Response` if the whole
  call succeeds (transactional apply) — a script that errors partway
  through never leaves a half-mutated request/response in flight. Every
  call is wrapped in `recover()` and a compile/runtime/panic error is
  logged-and-skipped by the caller (fail open) — one broken addon must
  never break a request that would otherwise have worked fine without it.
  `http.go`'s `http_request(url, method="GET", headers=None, body=None)`
  is the one deliberate hole in the sandbox — real outbound HTTP, e.g. to
  fetch a session cookie from an auth endpoint before rewriting a request
  — registered as a predeclared builtin (`addonPredeclared`) passed into
  `starlark.ExecFile` alongside the script's own top level. It returns a
  `*responseValue` (the same type `flow.response` is) built by reading the
  whole body eagerly and closing the real connection immediately, rather
  than `responseValue`'s usual lazy `loadBody` — nothing else is ever going
  to touch this one to read/close it the way the reverse-proxy pipeline
  does for the real `flow.response`, so leaving it lazy would leak the
  connection on any script that never touches `.text`/`.content`. A fixed
  `httpRequestTimeout` (10s) on the shared `httpClient` is what actually
  bounds a call's wall-clock time — `thread.SetMaxExecutionSteps` only
  counts interpreted Starlark steps, so it does nothing to stop a slow
  remote server from hanging the calling goroutine. Deliberately no further
  sandboxing (no URL allowlist, no private-IP/SSRF blocking): the operator
  writing an addon already has full control over this daemon's config and
  host, so this sandbox exists to contain a *buggy* script, not to defend
  against an *adversarial* one written by its own author.
- **`internal/edge`** — the daemon's presence at the *public* network edge:
  one standalone HTTPS reverse-proxy server per `sites[].serve`-enabled
  site, entirely independent of `internal/proxy`'s CONNECT/`intercept_ssl`
  machinery (no PAC-configured device talks to this; the two packages
  share only `internal/addon`). `Manager.Reconcile()` starts/stops/rebinds
  one listener per site exactly like `proxy.Manager` does per host.
  Certificates come from `github.com/caddyserver/certmagic` via DNS-01
  only (`DisableHTTPChallenge`/`DisableTLSALPNChallenge`, so no port 80/443
  exposure is needed for issuance itself — see `internal/edge/dns.go` for
  the `dns_provider` → `libdns.DNSProvider` switch, currently just
  `github.com/libdns/cloudflare`, an intentionally small string-switch
  rather than a plugin registry so adding a second provider stays a small,
  additive change). Each site gets its **own** `certmagic.Config` even
  though they share one `certmagic.Cache`/`certmagic.Storage` — required,
  not just tidy: certmagic calls its `GetConfigForCert` callback again at
  every renewal (weeks after this process's initial in-memory state), so
  the per-domain `certmagicSource.configs` map is what lets renewal find
  the right DNS provider/credentials again rather than a bare Config with
  no DNS solver configured. Renewal itself needs no code of its own: every
  `certmagic.Cache` runs its own background maintenance goroutine
  (`maintainAssets`, started once inside `certmagic.NewCache`) for the
  life of the process, periodically renewing anything in the cache marked
  `managed` — which `cfg.ManageSync` already does — well before expiry;
  `certmagicSource` doesn't need its own renewal loop or timer.
  `SiteServe.ACMEStaging` swaps `certmagic.ACMEIssuer.CA` from
  `LetsEncryptProductionCA` to `LetsEncryptStagingCA`, nothing else — same
  DNS-01 solver, same renewal path, just pointed at Let's Encrypt's
  much-higher-rate-limit, not-publicly-trusted directory. A site's
  certificate is obtained
  (`cfg.ManageSync`, blocking, bounded by `acmeTimeout`) **before** its
  listener binds — a public listener should never accept a connection it
  can't terminate TLS for — inside its own goroutine per site, so one
  site's slow/failing ACME issuance never delays any other site, the
  webui mux, or `proxy.Manager`'s hosts from starting; a failure is
  logged and retried on the next `Reconcile()` tick (the existing 3s
  config-poll loop in `main.go`), not through bespoke retry machinery.
  `Manager.certFunc` is a plain `CertFunc` function value rather than an
  interface — same pattern as `internal/proxy.NewHostHandler`'s `caGetter`
  parameter — specifically so `NewManagerForTesting` can inject a fake
  (a throwaway self-signed cert) and exercise the reverse-proxy/addon
  pipeline in tests with zero ACME/network involvement.
  `internal/edge/reverseproxy.go`'s `httputil.ReverseProxy` runs each
  addon's `request(flow)` hook (in order) from `Director` and each addon's
  `response(flow)` hook (in **reverse** order, mitmproxy convention) from
  `ModifyResponse` — which must never itself return a non-nil error, since
  `ReverseProxy` would otherwise replace the response with a generic error
  page, defeating the whole fail-open point.
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
  before calling this; `internal/webui`'s `/certs` listing/serving also
  excludes the key's filename outright, so even a `certs_dir` that's
  configured to overlap `ConfigDir` can't leak it. Loading a CA from disk
  validates it first — `IsCA`/`BasicConstraintsValid`, a self-signature
  check, the private key's public half actually matching the certificate,
  and the key file not being group/world-readable — since a
  parseable-but-mismatched pair or a loosely-permissioned key would start
  up fine while quietly breaking the trust model. `CA.LeafCertificate`/
  `CertificateFor` issue and cache (by hostname) a fresh leaf certificate
  signed by that CA per SNI, expiry-aware (an entry past its `NotAfter` is
  regenerated rather than served stale) and capped at
  `maxLeafCacheEntries` (dropping the whole cache rather than tracking
  per-entry LRU, since it's cheap to regenerate) so arbitrary client SNI
  values can't grow it forever; an IP-literal hostname (no SNI, or a
  `https://<ip>` CONNECT target) goes into the leaf's `IPAddresses`, not
  `DNSNames`, since that's what TLS verifiers require for IP literals.
  This is what lets `handleConnectIntercept` present a trusted-once-the-CA-
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
