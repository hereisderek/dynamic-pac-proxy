# dynamic-pac-proxy

Small Go service for a homelab LXC container that sits in front of Charles
(or any HTTP(S) proxy, e.g. [mitmproxy](https://mitmproxy.org)) running on
a machine whose IP moves around — found via mDNS/Bonjour (e.g.
`dereks-MacBook-Pro.local`), or at a fixed `host_ip` for a machine that
already has a static address.

Each configured host gets its own **fixed** listening port on this box.
Devices point their proxy settings at that fixed address once, forever —
never at Charles's own (dynamic) IP. This box then forwards every request
onward: to Charles, if it's currently reachable, or straight to the
destination (`DIRECT`) if it's not. Because the address devices use never
changes, there's nothing for a device to cache-and-go-stale on: it's not
re-reading a PAC file to learn a new IP, it's just always talking to this
box, which makes the online/offline call itself, per request, in real time.

## How it works

- Each host in `hosts:` gets a dedicated `server_port` on this box. A
  request arriving on that port is handled as a real forward proxy:
  - **CONNECT** (HTTPS): the client connection is hijacked, an upstream
    tunnel is established — chained through Charles with our own CONNECT
    if it's reachable, or dialed straight to the target if not — and bytes
    are spliced between the two, so TLS still terminates at the client and
    the real destination, unmodified.
  - **Plain HTTP proxy requests**: forwarded via `Charles:port` as the
    upstream proxy if reachable, or fetched directly otherwise.
- Reachability itself is a lazy, cached check per host, not a constant
  background poll: a request only triggers a real mDNS resolve + TCP dial
  if the cached result is older than that host's `refresh_interval`;
  otherwise the cached result is used immediately. Concurrent requests
  arriving right as the cache goes stale are coalesced into a single
  check, not one each.
- If a request that was sent chained through Charles actually fails to
  reach it (Charles just went down, mid-`refresh_interval`), the cache is
  invalidated immediately instead of waiting out the rest of
  `refresh_interval` — so the next request re-checks right away. This is
  itself rate-limited by `failure_cooldown`: a burst of requests all
  failing in the same window forces one early recheck, not one per
  request. A `DIRECT`-path failure (the real destination being down,
  unrelated to Charles) never triggers this.
- A separate poll (every 3s, fixed) checks the config file's mtime and
  hot-reloads it on change, starting/stopping/rebinding each host's
  listener as hosts are added/removed/changed — see "Configuration" below.
- `GET /` — a page listing every configured host's PAC URL and manual
  proxy address, so you don't have to construct them by hand — see
  "Point a device at it" below.
- `GET /proxy/<name>.pac` — a PAC file pointing at this box's own fixed
  `advertise_host:server_port` for that host. Static: it never needs to
  change, since reachability is handled behind it, not by it.
- `GET /status` — JSON array, one entry per host, with its last resolved
  IP, reachability, and last error. Querying it also counts as "a request
  came in", so it can trigger a check the same way live traffic does.
- `GET /certs` — a page listing any certificate files placed in the certs
  directory, with download links and per-platform install instructions —
  see "SSL certificates" below.
- Every proxied request (plain HTTP and CONNECT alike) is logged with the
  client device's own address (`r.RemoteAddr`) alongside the requested
  host and whether it went chained-through-Charles or DIRECT — see
  "Access logging" below.

## Project layout

```
main.go              thin entrypoint: flags, wiring, the top-level HTTP mux
internal/
  config/             YAML schema, hot-reload, config/certs path resolution
  health/             lazy TTL-gated reachability cache
  proxy/               the forward-proxy handler + per-host listener manager
  mdns/               raw mDNS (RFC 6762) A-record resolver
  mitm/               this tool's own local CA + per-host leaf cert issuance
  webui/              /proxy/<name>.pac, /status, /certs
  install/            --install/--uninstall (systemd/OpenRC)
tests/
  config/, health/, proxy/, webui/, mitm/   one test package per internal/ package
deploy/               example config.yaml + systemd/OpenRC/OpenWRT unit files
openwrt/              self-contained OpenWRT UCI+LuCI package (see its own README)
certs/                drop your exported Charles root certificate here
```

`main.go` stays at the repo root (rather than under `cmd/`) because it
`go:embed`s `deploy/config.yaml` as the seed config for `--install` — embed
patterns can't use `..`, so the embedding package has to be an ancestor of
`deploy/`, and the root is the simplest one that is. Everything else lives
in `internal/` with a small, deliberate exported API between packages; see
CLAUDE.md for the full per-package breakdown and the reasoning behind the
design (lazy health checks, fixed listen ports, etc.).

Tests are black-box: each `tests/<pkg>/*_test.go` file is `package
<pkg>_test`, importing `internal/<pkg>` and exercising only what it
exports — this is what keeps tests physically separate from the code they
test while still being able to reach package internals that matter (e.g.
`health.State.SetSnapshot` to seed a fake reachability result).

## Build

Requires Go 1.21+ on whatever machine you build on (cross-compile for the
LXC container if needed).

```sh
go mod tidy   # fetches dependencies and writes go.sum
go test ./...
go build -o dynamic-pac-proxy .

# Cross-compile for a typical Proxmox LXC (amd64):
GOOS=linux GOARCH=amd64 go build -o dynamic-pac-proxy .
```

Cross-compiling (`GOOS=linux`) disables cgo by default, which is what you
want here: the binary comes out fully static and works unmodified on both
glibc distros (Debian, Ubuntu, Fedora, etc.) and musl-based ones like
Alpine — no extra build tags or flags needed.

## Configuration

Settings live in a YAML file. The path is chosen in this order:

1. `--config <path>`, if given — used as-is.
2. `/etc/dynamic-pac-proxy/config.yaml`, if that file exists.
3. `config.yaml` next to the binary, if that file exists.
4. Otherwise falls back to `/etc/dynamic-pac-proxy/config.yaml` (which
   won't exist at this point, so the service starts with a single
   built-in default host).

Whichever path is chosen is logged at startup (`using config file: ...`)
along with which rule picked it. See `deploy/config.yaml` for the full
example:

```yaml
listen_addr: ":8080"
advertise_host: "192.168.1.50"   # this box's LAN IP/hostname, as seen by client devices

# Defaults applied to any host below that doesn't override them.
refresh_interval: 15s
mdns_timeout: 2s
dial_timeout: 1s
failure_cooldown: 5s

hosts:
  - name: derek-macbook
    host_name: dereks-MacBook-Pro.local
    host_port: 8888    # Charles's port on that Mac
    server_port: 8081  # THIS box's fixed port for that host

  - name: office-pc
    host_name: office-desktop.local
    host_port: 9999
    server_port: 8082
    refresh_interval: 5s   # optional per-host override
    dial_timeout: 500ms    # optional per-host override

  - name: mitmproxy-box
    host_ip: "172.16.2.23"   # fixed IP instead of host_name — see below
    host_port: 8080
    server_port: 8083
```

Top-level fields:

| Field              | Default          | Meaning                                              |
|---------------------|------------------|----------------------------------------------------|
| `listen_addr`       | `:8080`          | Where the HTTP status/PAC server listens (not the proxy ports — see `server_port` below) |
| `advertise_host`    | auto-detected LAN IP | The address baked into PAC files at `/proxy/<name>.pac`; set explicitly if the auto-detected guess isn't what devices can actually reach (e.g. multi-homed box) |
| `refresh_interval`  | `15s`            | Default health-check cache TTL for hosts that don't override it |
| `mdns_timeout`      | `2s`             | Default mDNS reply timeout for hosts that don't override (unused for `host_ip` hosts — nothing to resolve) |
| `dial_timeout`      | `1s`             | Default TCP dial timeout for hosts that don't override |
| `failure_cooldown`  | `5s`             | Default minimum spacing between failure-triggered early rechecks, for hosts that don't override |
| `certs_dir`         | a `certs` folder next to `config.yaml` | Directory served at `/certs` for downloading/installing Charles's SSL certificate — see "SSL certificates" below |
| `addons_dir`        | an `addons` folder next to `config.yaml` | Directory `sites[].addons` script paths are resolved relative to — see "Sites: public reverse-proxy mirrors" below |
| `hosts`             | (one host, see `deploy/config.yaml`) | The list of hosts to proxy for |
| `sites`             | (none)           | Optional list of addon-scripted reverse-proxy mirrors — see "Sites: public reverse-proxy mirrors" below |

Each entry in `hosts`:

| Field              | Required | Meaning                                                |
|---------------------|----------|-----------------------------------------------------------|
| `name`              | yes      | Identifier used in URLs/status (`[a-zA-Z0-9_-]+`, unique)|
| `host_name`         | one of these two | Bonjour hostname to resolve, e.g. `some-machine.local` |
| `host_ip`           | one of these two | Fixed IP instead of a Bonjour hostname — skips mDNS resolution entirely. Use this for a machine with a static/reserved address, or one that doesn't answer mDNS at all (e.g. a [mitmproxy](https://mitmproxy.org) instance) |
| `host_port`         | yes      | The port Charles (or whatever proxy) listens on, on that host |
| `server_port`       | yes      | The fixed port **this box** listens on for this host — point devices here. Must be unique across hosts and different from `listen_addr`'s port |
| `refresh_interval`  | no       | Overrides the top-level default for this host only       |
| `mdns_timeout`      | no       | Overrides the top-level default for this host only       |
| `dial_timeout`      | no       | Overrides the top-level default for this host only       |
| `failure_cooldown`  | no       | Overrides the top-level default for this host only       |
| `intercept_ssl`     | no       | Terminate HTTPS at this box instead of tunneling it opaquely — see "SSL interception" below. Default `false` |

**Hot reload:** editing `hosts` (adding, removing, or changing any host's
settings), or the top-level defaults, takes effect within a few seconds
automatically — no restart needed. Adding a host starts a new proxy
listener for it; removing one stops its listener and drops it from
`/status`; changing `server_port` rebinds it to the new port. Only the
top-level `listen_addr` (the status/PAC server, not the per-host proxy
ports) needs a restart to take effect — see "Deploy as an auto-start
service" below.

If the config file doesn't exist at startup, the service runs with a
single built-in default host and logs that it did so. If the file exists
but fails to parse or validate at startup (e.g. a duplicate `name`, a
`host_port`/`server_port` out of range, a `server_port` collision between
two hosts or with `listen_addr`, an empty `hosts` list), the service
refuses to start — fail fast rather than run with an unintended config.
Once running, a bad edit (e.g. a YAML typo) is logged and ignored — the
service keeps using the last known-good config instead of crashing.

## Deploy as an auto-start service

### The easy way: `--install`

Copy the binary to its permanent location first (the generated service
will point at wherever the binary sits when you run `--install`), then
install and start it as a service in one step:

```sh
scp dynamic-pac-proxy root@<host>:/usr/local/bin/dynamic-pac-proxy
ssh root@<host> 'chmod +x /usr/local/bin/dynamic-pac-proxy && /usr/local/bin/dynamic-pac-proxy --install'
```

`--install` (must be run as root):

- Detects the init system — `systemd` (checks for `/run/systemd/system`)
  or OpenRC (checks for `rc-service` on `PATH`, i.e. Alpine, Gentoo) — and
  writes + enables + starts the matching service definition
  (`deploy/systemd/dynamic-pac-proxy.service` or `deploy/openrc/dynamic-pac-proxy`,
  rendered with the actual binary/config paths).
- Resolves the config path the same way the service itself does (see
  "Configuration" above): pass `--config <path>` to pin it explicitly, or
  omit it to use `/etc/dynamic-pac-proxy/config.yaml` / the binary's
  folder / built-in defaults.
- If nothing exists at the resolved config path yet, seeds it with the
  example config (`deploy/config.yaml`) so there's something to edit —
  it never overwrites a config file that's already there.

```sh
sudo dynamic-pac-proxy --install                          # auto-detect config location
sudo dynamic-pac-proxy --install --config /path/to/my.yaml # pin it explicitly
```

To remove it again:

```sh
sudo dynamic-pac-proxy --uninstall
```

This stops and disables the service and deletes its service definition
(the systemd unit or OpenRC script) — it leaves your config file at
`/etc/dynamic-pac-proxy/` untouched, since that's your data.

Unsupported init system (neither systemd nor OpenRC detected)? `--install`
and `--uninstall` exit with an error explaining that, and the manual steps
below still work anywhere.

### Manual install

If you'd rather not run the binary as root to install itself, or you're
on an init system `--install` doesn't recognize, wire it up by hand:

```sh
scp dynamic-pac-proxy root@<host>:/usr/local/bin/dynamic-pac-proxy
ssh root@<host> 'mkdir -p /etc/dynamic-pac-proxy'
scp deploy/config.yaml root@<host>:/etc/dynamic-pac-proxy/config.yaml
ssh root@<host> 'chmod +x /usr/local/bin/dynamic-pac-proxy'
```

Then set up the service using whichever init system the distro runs.

#### Alpine Linux (OpenRC)

Alpine (and other OpenRC-based distros, e.g. Gentoo) doesn't have
`systemd`; use the OpenRC script in `deploy/openrc/`:

```sh
scp deploy/openrc/dynamic-pac-proxy root@<host>:/etc/init.d/dynamic-pac-proxy
ssh root@<host> 'chmod +x /etc/init.d/dynamic-pac-proxy \
  && rc-update add dynamic-pac-proxy default \
  && rc-service dynamic-pac-proxy start'
```

Check it came up and see its logs:

```sh
rc-service dynamic-pac-proxy status
tail -f /var/log/dynamic-pac-proxy.log
```

#### systemd-based distros (Debian, Ubuntu, Fedora, RHEL/CentOS, Arch, openSUSE, ...)

```sh
scp deploy/systemd/dynamic-pac-proxy.service root@<host>:/etc/systemd/system/
ssh root@<host> 'systemctl daemon-reload && systemctl enable --now dynamic-pac-proxy'
```

Check it came up and see its logs:

```sh
systemctl status dynamic-pac-proxy
journalctl -u dynamic-pac-proxy -f
```

#### OpenWRT routers

`--install` doesn't recognize OpenWRT's init system (`procd`) yet, so this
one's manual — but running it directly on the router is arguably the best
place for it anyway: the router already sees all your devices' traffic on
the LAN, so there's no L2/VLAN bridging question the way there is for an
LXC container (see the networking caveat below), as long as the proxy
port(s) aren't firewalled off from the LAN zone (they aren't, by default).

**Want a LuCI web UI instead of hand-editing `config.yaml`?** See
[`openwrt/`](openwrt/) — a self-contained UCI config + LuCI app (start/
stop/restart, edit hosts, live status) plus a script that builds a real
installable `.ipk`. The steps below are the bare-metal version with no
GUI; `openwrt/README.md` covers the packaged version.

**1. Find the router's architecture**, since it's essentially never amd64:

```sh
ssh root@<router> 'opkg print-architecture'
# or, cruder but usually enough:
ssh root@<router> 'uname -m'
```

Common results and the matching Go build:

| Router arch                          | Go build                                    |
|----------------------------------------|----------------------------------------------|
| `mipsel`, `mips_24kc` (most Broadcom/Atheros/MediaTek MIPS routers) | `GOOS=linux GOARCH=mipsle go build` |
| `mips`, big-endian MIPS (rarer)        | `GOOS=linux GOARCH=mips go build`           |
| `aarch64` (newer routers: GL.iNet, some Linksys/Netgear)            | `GOOS=linux GOARCH=arm64 go build`  |
| `arm`, `armv7` (older ARM routers)      | `GOOS=linux GOARCH=arm GOARM=7 go build`    |

If you're not sure which ARM variant, `GOARM=5` is the safest/slowest
fallback; try `7` first and drop down only if the binary fails to run.

**2. Build small.** Flash space on routers is often measured in tens of
MB, not GB — strip debug symbols to cut the binary down:

```sh
GOOS=linux GOARCH=mipsle go build -ldflags="-s -w" -o dynamic-pac-proxy-mipsle .
ssh root@<router> 'df -h /'   # check free space before copying over
```

**3. Copy the binary, config, and init script.** `/etc/dynamic-pac-proxy/`
matches this binary's own built-in default config location, so no
`--config` flag is needed in the init script:

```sh
ssh root@<router> 'mkdir -p /etc/dynamic-pac-proxy'
scp dynamic-pac-proxy-mipsle root@<router>:/usr/bin/dynamic-pac-proxy
scp deploy/config.yaml root@<router>:/etc/dynamic-pac-proxy/config.yaml
scp deploy/openwrt/dynamic-pac-proxy root@<router>:/etc/init.d/dynamic-pac-proxy
ssh root@<router> 'chmod +x /usr/bin/dynamic-pac-proxy /etc/init.d/dynamic-pac-proxy'
```

**4. Enable and start it:**

```sh
ssh root@<router> '/etc/init.d/dynamic-pac-proxy enable && /etc/init.d/dynamic-pac-proxy start'
```

Check it came up and see its logs (procd logs go through `logread`, not a
file):

```sh
ssh root@<router> 'service dynamic-pac-proxy status; logread | grep dynamic-pac-proxy'
```

If devices on the LAN can't reach the proxy port, check the firewall zone
the router's LAN interface is in — `uci show firewall` — the default LAN
zone's input policy is normally `ACCEPT`, but a hardened config may need
an explicit rule opening the `listen_addr`/`server_port`s to the LAN zone.

#### Anything else

The binary is a single static executable that takes no more than
`--config <path>` (see "Configuration" above for how that path is chosen
if you omit the flag) — wiring it into any other supervisor (runit, s6,
a plain `@reboot` cron line, etc.) just means pointing that supervisor's
"run this command, restart it if it dies" mechanism at the binary; there's
nothing distro-specific in the binary itself.

### After it's running

Edit `/etc/dynamic-pac-proxy/config.yaml` on the box any time afterward —
changes to the host list, hostnames, ports, or timeouts apply live, no
restart needed (see "Hot reload" above). Only a change to `listen_addr`
needs a restart (`rc-service dynamic-pac-proxy restart` on Alpine,
`systemctl restart dynamic-pac-proxy` on systemd).

### Important networking caveat

mDNS relies on multicast UDP to `224.0.0.251:5353` on the local network
segment. This only works if wherever this binary runs shares an L2/VLAN
with the Mac. Running it on the router itself (see "OpenWRT routers"
above) trivially satisfies this, since the router's LAN interface is that
segment. Running it in an LXC container needs that container's NIC
bridged onto the same L2/VLAN as your Mac (the typical Proxmox `vmbr0`
setup) — if the container is instead behind NAT (e.g. a separate routed
subnet), multicast won't reach it and resolution will fail. In that case
you'd need an mDNS reflector/repeater on the network, or sidestep mDNS
entirely for that host by giving it a static IP / DHCP reservation and
using `host_ip` instead of `host_name` in its config entry.

## Point a device at it

Either configure the device's proxy manually with `advertise_host:server_port`
(e.g. `192.168.1.50:8081`), or use "Automatic Proxy Configuration" / PAC URL
pointed at:

```
http://<lxc-host-ip>:8080/proxy/<name>.pac
```

Visit `http://<lxc-host-ip>:8080/` for a page listing both of these,
already filled in, for every configured host — no need to construct
either URL by hand or remember each host's `server_port`.

Either way, this is a one-time setup: the address never needs to be
re-fetched or changed. Whether Charles is currently reachable is decided
per request, on this box, not by anything the device caches.

## Access logging

Every proxied request is logged (to stdout, which lands in
`/var/log/dynamic-pac-proxy.log` under OpenRC — see "Current deployment" in
CLAUDE.md) with the originating device's own address, not Charles's or the
target's:

```
host "derek-macbook": GET http://example.com/ from 192.168.1.42 -> chained via 192.168.1.30:8888
host "derek-macbook": CONNECT example.com:443 from 192.168.1.42 -> chained
```

With `intercept_ssl` enabled for a host (see "SSL interception" below),
the CONNECT line above becomes a real request line with the actual path —
`GET https://example.com/some/page from 192.168.1.42 -> DIRECT` — instead
of just the bare `host:443` a plain tunnel can see.

This works because devices are configured to talk straight to this box
(per the PAC/manual proxy setup above) — `r.RemoteAddr` on every request is
therefore always the real client device, never a hop through Charles. If
you were previously trying to answer "who visited what" purely from
Charles's own logs, that's exactly what was missing: chained requests reach
Charles *from this box*, so Charles's own logs show this box's IP as the
source, not the original device's. Cross-reference the logged IP with your
router's DHCP client list (or set static/reserved LAN IPs for known
devices) to turn it into a device name.

## SSL certificates

For Charles to man-in-the-middle HTTPS traffic (SSL Proxying), each client
device needs to install and trust Charles's own root certificate. `GET
/certs` (same port as `/status` and `/proxy/<name>.pac`, e.g.
`http://172.16.2.22:8080/certs`) serves a page listing whatever certificate
files you've placed in the certs directory, with download links and
per-platform (iOS/Android/macOS/Windows) install instructions.

The certificate itself is never bundled with this binary — Charles
generates its own root certificate per install, so there's no single file
that would work for everyone. Export yours from Charles
(**Help > SSL Proxying > Save Charles Root Certificate...**) and drop it
into the certs directory; see `certs/README.md` for details. The directory
defaults to a `certs` folder next to `config.yaml` (override with
`certs_dir`), and — like the config file itself — is read fresh on every
request, so dropping in a new certificate doesn't need a restart.

Check `http://<lxc-host-ip>:8080/status` any time to see what each
configured host currently resolved to and whether it's reachable.

## SSL interception

Set `intercept_ssl: true` on a host to have **this tool** terminate HTTPS
for it, instead of tunneling opaque encrypted bytes end to end:

1. When a client CONNECTs for that host, this box presents its own
   certificate for the requested domain — issued on the fly, signed by
   dynamic-pac-proxy's own local CA ("Dynamic PAC Proxy Local CA") — and
   completes the TLS handshake with the client itself.
2. With the traffic now decrypted, the real HTTP request (method, full
   path, headers) goes through the exact same chained-vs-DIRECT logic as
   plain HTTP requests — see "How it works" — which means access logging
   now shows the real URL for HTTPS traffic too, not just the CONNECT
   `host:443`.
3. It's then re-encrypted over a brand new, real TLS connection to
   whatever it's forwarded to (chained through Charles's own CONNECT, or
   straight to the real destination) — nothing between this box and the
   real destination ever goes out in the clear.

For this to work, every client device needs to install and trust this
tool's own CA — its public certificate is generated automatically (once,
on first use of `intercept_ssl` anywhere) and shows up on the `/certs`
page alongside any Charles certificate you've dropped in, as
`dynamic-pac-proxy-ca.pem`; see `certs/README.md`. The matching private
key is written next to `config.yaml` (never under `certs/`, never served)
— back it up along with your config if you don't want every device to
need re-trusting after a fresh install.

**Caveats:**

- Nothing is generated or touched on disk until some host actually needs
  it — same lazy-on-first-use design as the health checks.
- The client-facing side only speaks HTTP/1.1 (no HTTP/2) — this tool
  doesn't offer `h2` in its ALPN response, so browsers negotiate HTTP/1.1
  with it instead. The upstream/outbound leg is unaffected by this.
- If a host has both `intercept_ssl: true` *and* Charles's own SSL
  Proxying enabled for the same domain, you'll get a double interception:
  this tool decrypts first, then re-encrypts a fresh TLS connection that
  gets tunneled through Charles via CONNECT — at which point Charles's own
  SSL Proxying would try to intercept *that* connection too, presenting
  *its* certificate to this tool's outbound request. This tool doesn't
  trust Charles's CA for outbound connections (only the system's normal
  roots), so that combination will fail with a certificate error. Use one
  or the other for a given host, not both.
- A destination with its own self-signed/private certificate (not signed
  by a public or system-trusted CA) will fail the same way any HTTPS
  client would — this tool verifies the real destination's certificate
  against the normal system trust store on the outbound leg; it doesn't
  weaken that check.

## Sites: public reverse-proxy mirrors with addon scripts

A `sites:` entry with a `serve:` block turns this daemon into a standalone,
publicly-reachable HTTPS reverse-proxy for a domain **you actually own**
(unlike `intercept_ssl` above, which only works for domains you don't own
because it relies on a locally-trusted CA) — fronted by a real Let's
Encrypt certificate, forwarding to a real upstream, with small "addon"
scripts able to rewrite the request/response in transit (inject headers,
rewrite body content, etc.).

```yaml
sites:
  - name: netflix
    addons:
      - "netflix/cookie.star"   # relative to addons_dir; run in order for requests, reverse order for responses
    serve:
      domain: netflix.mydomain.com   # a domain you own — the ACME cert's SAN and expected SNI
      listen_addr: ":8443"           # where THIS daemon binds — see "Network topology" below
      backend: "https://www.netflix.com"
      acme_email: you@mydomain.com
      dns_provider: cloudflare
      cloudflare:
        api_token_env: "CLOUDFLARE_API_TOKEN"   # names an env var — never put the token itself here
        # api_token: "..."   # or a literal token instead — see the field table below
      # acme_staging: true   # optional — use Let's Encrypt's staging directory instead of production
```

Each entry in `sites`:

| Field                        | Required | Meaning |
|------------------------------|----------|---------|
| `name`                       | yes      | Identifier (`[a-zA-Z0-9_-]+`, unique) |
| `addons`                     | no       | Addon script paths, relative to `addons_dir` — see "Addon scripts" below |
| `serve.domain`                | yes*     | The public domain this site serves — a domain you own and control DNS for |
| `serve.listen_addr`           | yes*     | Where this daemon binds for this site, e.g. `:8443` |
| `serve.backend`               | yes*     | Full URL of the real upstream to reverse-proxy to |
| `serve.acme_email`            | yes*     | Contact email for the Let's Encrypt account (expiry/account notices) |
| `serve.dns_provider`          | yes*     | DNS-01 challenge provider — only `cloudflare` today, more planned |
| `serve.cloudflare.api_token_env` | yes*† | Name of the environment variable holding a scoped Cloudflare API Token (Zone:DNS:Edit) — must actually be set, checked at config-load time |
| `serve.cloudflare.api_token`  | yes*†    | The Cloudflare API Token itself, given literally instead of via an env var. Mutually exclusive with `api_token_env` — treat a config file using this like you would a file holding the token in plaintext, because it is one |
| `serve.acme_staging`          | no       | `true` routes issuance/renewal through Let's Encrypt's **staging** directory — much higher rate limits, but the issued cert isn't trusted by real clients. For exercising the DNS-01 pipeline itself without burning production's rate limits; never leave this on for a site real clients depend on |

† if `dns_provider: cloudflare`, exactly one of `serve.cloudflare.api_token` / `api_token_env` is required.

\* only required if `serve` is set at all — a site with no `serve` block
currently has no effect (a future release will let such a site attach to
an `intercept_ssl` host instead, for domains you don't own).

**Network topology:** this daemon terminates TLS itself with its own ACME
certificate — it does **not** expect something else (a reverse proxy,
load balancer, CDN) to already be doing that for `serve.domain`, since that
would defeat the point of holding a real certificate here at all. The
expected setup is: your own edge reverse proxy does **TCP/SNI passthrough**
for `serve.domain` to `serve.listen_addr` on this box (port 80 is never
needed — see "Why DNS-01" below), so the ACME-issued certificate this
daemon presents is the one real clients actually see.

**Why DNS-01, not HTTP-01:** HTTP-01 challenges require port 80 reachable
from the public internet at issuance time, every ~60-90 days. DNS-01
instead requires API credentials for whatever DNS provider hosts your
domain (used to create a temporary TXT record proving ownership) but never
needs any inbound port opened for issuance itself — a better fit for a
homelab box that's otherwise not directly internet-facing.

**Certificate/account storage:** everything ACME needs — the account
private key it generates, plus issued certs and their keys — lives under
an `acme-cache` folder next to `config.yaml` (i.e. under the same directory
as `internal/mitm`'s CA private key, for the same reason: never served,
never under `certs_dir`). Back it up along with your config to avoid
re-issuing certificates (and burning into Let's Encrypt's rate limits)
after a fresh install.

**First boot / renewal:** a site's public listener doesn't bind until its
certificate is actually obtained — a public listener should never accept a
connection it can't terminate TLS for. A site that fails to get a
certificate (bad credentials, a DNS/API hiccup, a Let's Encrypt rate
limit) is retried automatically every few seconds (the same poll that
picks up other config changes) and logged — it never blocks any other
site, host, or the rest of the daemon from starting. Renewal itself is
also fully automatic once issued: certmagic runs a background maintenance
loop for as long as this process is up, renewing each site's certificate
well before it expires — nothing in `config.yaml` needs to change or
reload to trigger it.

### Addon scripts

Addon scripts are [Starlark](https://github.com/bazelbuild/starlark) — a
small, sandboxed, Python-syntax **subset** used by Bazel — not real Python,
and not real [mitmproxy](https://mitmproxy.org) compatibility, even though
the shape is deliberately close to a simple mitmproxy addon so one can be
hand-ported easily:

```python
# addons/netflix/cookie.star
def request(flow):
    flow.request.headers["X-Injected"] = "1"

def response(flow):
    flow.response.text = flow.response.text.replace("SECRET", "REDACTED")
    flow.response.headers["X-Modified"] = "yes"
```

- `flow.request` / `flow.response` expose `method` (request only), `url`
  (request only — reassigning it rewrites the outbound request),
  `status_code` (response only), `headers` (a dict — a header with one
  value reads/writes as a plain string, one with several, e.g. repeated
  `Set-Cookie`, as a list of strings; delete with
  `flow.request.headers.pop("X-Foo", None)`, since Starlark has no `del`
  statement), and `text`/`content` (string/bytes; only read the body if
  your addon actually needs to inspect or rewrite it — untouched bodies
  are never buffered).
- A script needs only the hook(s) it uses — a request-only or
  response-only script is fine.
- **Addons must be stateless across requests.** Compiled scripts are
  cached and their top-level values frozen for safe concurrent reuse — a
  script that tries to keep mutable state between calls (a module-level
  counter, say) gets a loud "cannot mutate frozen value" error instead of
  a silent race.
- **One broken addon never breaks a request.** A script that fails to
  compile, or whose `request()`/`response()` call errors partway through,
  is logged and skipped — as if it weren't configured for that request at
  all; nothing it already mutated is applied.
- **Large responses:** `flow.response.text`/`content` necessarily buffers
  the whole response body in memory. Fine for API/HTML/JSON traffic; if a
  site's backend also serves bulk media/video, check `flow.request.url`
  (or `flow.response.headers`) inside the script and return early without
  touching `.text`/`.content` for paths/content-types you don't actually
  need to rewrite.
- No filesystem, network, or `import` access is available to a script —
  the `flow` object is the entire capability surface it gets.
