# dynamic-pac-proxy

Small Go service for a homelab LXC container. For each configured host
(e.g. your Mac at `dereks-MacBook-Pro.local` running Charles on 8888), it
keeps track of the current IP (found via mDNS/Bonjour) and confirms the
proxy port is actually listening, and serves PAC files so test devices
always route through the right IP — falling back to `DIRECT` if a host or
its proxy isn't up.

## How it works

- Every configured host gets its own background refresh loop (interval
  configurable per-host, default 15s) that sends a raw mDNS A-record query
  for that host's hostname and, if it resolves, does a quick TCP dial to
  confirm the proxy port is listening.
- Each host's result is cached in memory. HTTP requests never block on
  mDNS — they just read the cached state, so PAC responses are instant.
- A separate poll (every 3s, fixed) checks the config file's mtime and
  hot-reloads it on change, starting/stopping refresh loops as hosts are
  added/removed — see "Configuration" below.
- `GET /proxy/<name>.pac` — PAC for one specific configured host.
- `GET /proxy.pac` — combined PAC: the first host (in config order) that's
  currently reachable, or `DIRECT` if none are.
- `GET /status` — JSON array, one entry per host, with its resolved IP,
  reachability, and last error — useful for debugging from a browser or
  curl.

## Build

Requires Go 1.21+ on whatever machine you build on (cross-compile for the
LXC container if needed).

```sh
go mod tidy   # fetches dependencies and writes go.sum
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

# Defaults applied to any host below that doesn't override them.
refresh_interval: 15s
mdns_timeout: 2s
dial_timeout: 1s

hosts:
  - name: derek-macbook
    mdns_hostname: dereks-MacBook-Pro.local
    port: 8888

  - name: office-pc
    mdns_hostname: office-desktop.local
    port: 9999
    refresh_interval: 5s   # optional per-host override
    dial_timeout: 500ms    # optional per-host override
```

Top-level fields:

| Field              | Default | Meaning                                              |
|---------------------|---------|--------------------------------------------------------|
| `listen_addr`       | `:8080` | Where this service listens                             |
| `refresh_interval`  | `15s`   | Default re-check interval for hosts that don't override |
| `mdns_timeout`      | `2s`    | Default mDNS reply timeout for hosts that don't override|
| `dial_timeout`      | `1s`    | Default TCP dial timeout for hosts that don't override  |
| `hosts`             | (one host, see `deploy/config.yaml`) | The list of hosts to track |

Each entry in `hosts`:

| Field              | Required | Meaning                                                |
|---------------------|----------|-----------------------------------------------------------|
| `name`              | yes      | Identifier used in URLs/status (`[a-zA-Z0-9_-]+`, unique)|
| `mdns_hostname`     | yes      | Bonjour hostname to resolve, e.g. `some-machine.local`   |
| `port`              | yes      | Port the proxy listens on for this host                  |
| `refresh_interval`  | no       | Overrides the top-level default for this host only       |
| `mdns_timeout`      | no       | Overrides the top-level default for this host only       |
| `dial_timeout`      | no       | Overrides the top-level default for this host only       |

**Hot reload:** editing `hosts` (adding, removing, or changing any host's
settings), or the top-level defaults, takes effect within a few seconds
automatically — no restart needed. Adding a host starts a new refresh loop
for it; removing one stops its loop and drops it from `/status`.
`listen_addr` is only read at startup, since changing it means rebinding
the HTTP listener; that one needs a service restart (see "Deploy as an
auto-start service" below).

If the config file doesn't exist at startup, the service runs with a
single built-in default host and logs that it did so. If the file exists
but fails to parse or validate at startup (e.g. a duplicate `name`, a
`port` out of range, an empty `hosts` list), the service refuses to start
— fail fast rather than run with an unintended config. Once running, a bad
edit (e.g. a YAML typo) is logged and ignored — the service keeps using
the last known-good config instead of crashing.

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
segment. This only works if the LXC container's NIC is bridged onto the
same L2/VLAN as your Mac (the typical Proxmox `vmbr0` setup). If the
container is behind NAT (e.g. a separate routed subnet), multicast won't
reach it and resolution will fail — in that case you'd need an mDNS
reflector/repeater on the network, or switch to a static IP / DHCP
reservation for the Mac instead of hostname resolution.

## Point a device at it

On the test device, set the network's proxy configuration to "Automatic
Proxy Configuration" / PAC URL and point it at either:

```
http://<lxc-host-ip>:8080/proxy.pac              # first reachable host wins
http://<lxc-host-ip>:8080/proxy/<name>.pac        # one specific host
```

Check `http://<lxc-host-ip>:8080/status` any time to see what each
configured host currently resolved to and whether its proxy is reachable.
