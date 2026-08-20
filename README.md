# dynamic-pac-proxy

Small Go service for a homelab LXC container that sits in front of Charles
(or any HTTP(S) proxy) running on a machine whose IP moves around — found
via mDNS/Bonjour (e.g. `dereks-MacBook-Pro.local`).

Each configured host gets its own **fixed** listening port on this box.
Devices point their proxy settings at that fixed address once, forever —
never at Charles's own (dynamic) IP. This box then forwards every request
onward: to Charles, if it's currently reachable, or straight to the
destination (`DIRECT`) if it's not. Because the address devices use never
changes, there's nothing for a device to cache-and-go-stale on: it's not
re-reading a PAC file to learn a new IP, it's just always talking to this
box, which makes the online/offline call itself, per request, in real time.

## How it works

- Each host in `hosts:` gets a dedicated `listen_port` on this box. A
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
- `GET /proxy/<name>.pac` — a PAC file pointing at this box's own fixed
  `advertise_host:listen_port` for that host. Static: it never needs to
  change, since reachability is handled behind it, not by it.
- `GET /status` — JSON array, one entry per host, with its last resolved
  IP, reachability, and last error. Querying it also counts as "a request
  came in", so it can trigger a check the same way live traffic does.

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
    mdns_hostname: dereks-MacBook-Pro.local
    port: 8888        # Charles's port on that Mac
    listen_port: 8081  # THIS box's fixed port for that host

  - name: office-pc
    mdns_hostname: office-desktop.local
    port: 9999
    listen_port: 8082
    refresh_interval: 5s   # optional per-host override
    dial_timeout: 500ms    # optional per-host override
```

Top-level fields:

| Field              | Default          | Meaning                                              |
|---------------------|------------------|----------------------------------------------------|
| `listen_addr`       | `:8080`          | Where the HTTP status/PAC server listens (not the proxy ports — see `listen_port` below) |
| `advertise_host`    | auto-detected LAN IP | The address baked into PAC files at `/proxy/<name>.pac`; set explicitly if the auto-detected guess isn't what devices can actually reach (e.g. multi-homed box) |
| `refresh_interval`  | `15s`            | Default health-check cache TTL for hosts that don't override it |
| `mdns_timeout`      | `2s`             | Default mDNS reply timeout for hosts that don't override |
| `dial_timeout`      | `1s`             | Default TCP dial timeout for hosts that don't override |
| `failure_cooldown`  | `5s`             | Default minimum spacing between failure-triggered early rechecks, for hosts that don't override |
| `hosts`             | (one host, see `deploy/config.yaml`) | The list of hosts to proxy for |

Each entry in `hosts`:

| Field              | Required | Meaning                                                |
|---------------------|----------|-----------------------------------------------------------|
| `name`              | yes      | Identifier used in URLs/status (`[a-zA-Z0-9_-]+`, unique)|
| `mdns_hostname`     | yes      | Bonjour hostname to resolve, e.g. `some-machine.local`   |
| `port`              | yes      | The port Charles (or whatever proxy) listens on, on that resolved host |
| `listen_port`       | yes      | The fixed port **this box** listens on for this host — point devices here. Must be unique across hosts and different from `listen_addr`'s port |
| `refresh_interval`  | no       | Overrides the top-level default for this host only       |
| `mdns_timeout`      | no       | Overrides the top-level default for this host only       |
| `dial_timeout`      | no       | Overrides the top-level default for this host only       |
| `failure_cooldown`  | no       | Overrides the top-level default for this host only       |

**Hot reload:** editing `hosts` (adding, removing, or changing any host's
settings), or the top-level defaults, takes effect within a few seconds
automatically — no restart needed. Adding a host starts a new proxy
listener for it; removing one stops its listener and drops it from
`/status`; changing `listen_port` rebinds it to the new port. Only the
top-level `listen_addr` (the status/PAC server, not the per-host proxy
ports) needs a restart to take effect — see "Deploy as an auto-start
service" below.

If the config file doesn't exist at startup, the service runs with a
single built-in default host and logs that it did so. If the file exists
but fails to parse or validate at startup (e.g. a duplicate `name`, a
`port`/`listen_port` out of range, a `listen_port` collision between two
hosts or with `listen_addr`, an empty `hosts` list), the service refuses
to start — fail fast rather than run with an unintended config. Once
running, a bad edit (e.g. a YAML typo) is logged and ignored — the service
keeps using the last known-good config instead of crashing.

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
an explicit rule opening the `listen_addr`/`listen_port`s to the LAN zone.

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
you'd need an mDNS reflector/repeater on the network, or switch to a
static IP / DHCP reservation for the Mac instead of hostname resolution.

## Point a device at it

Either configure the device's proxy manually with `advertise_host:listen_port`
(e.g. `192.168.1.50:8081`), or use "Automatic Proxy Configuration" / PAC URL
pointed at:

```
http://<lxc-host-ip>:8080/proxy/<name>.pac
```

Either way, this is a one-time setup: the address never needs to be
re-fetched or changed. Whether Charles is currently reachable is decided
per request, on this box, not by anything the device caches.

Check `http://<lxc-host-ip>:8080/status` any time to see what each
configured host currently resolved to and whether it's reachable.
