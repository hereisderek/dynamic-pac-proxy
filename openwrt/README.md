# OpenWRT package + LuCI app

This directory is self-contained: it packages the `dynamic-pac-proxy`
binary (built from the parent directory) together with a LuCI web UI and
an OpenWRT-native (UCI) config, so the router can be configured entirely
through its web interface instead of hand-editing `config.yaml`.

**Not yet tested against a real OpenWRT device or LuCI instance** — this
was built and verified as far as possible without router access (see
"What's verified, what isn't" below). Treat the first install as a test
and check the smoke-test checklist at the bottom of this file.

## How it fits together

```
UCI (/etc/config/dynamic-pac-proxy)  <-- edited by LuCI, or `uci set ...`
        |
        v  uci2yaml.sh (run by the init script on start/reload)
        |
/etc/dynamic-pac-proxy/config.yaml   <-- the format the Go binary actually reads
        |
        v
dynamic-pac-proxy (unmodified — doesn't know UCI exists)
```

The Go binary in the parent directory is completely unaware of UCI or
OpenWRT — it only ever reads `config.yaml`, exactly as it does on Alpine or
any other target. `uci2yaml.sh` is the only translation layer, and it's
plain POSIX shell using OpenWRT's standard `/lib/functions.sh` UCI
functions (`config_load`/`config_get`/`config_foreach`), the same API
every other OpenWRT package's init script uses.

Because the daemon already hot-reloads `config.yaml` on its own (polls its
mtime every few seconds — see the parent README), most UCI edits just need
`config.yaml` regenerated, not a process restart. The init script's
`reload_service()` does exactly that, and it's wired up to fire
automatically whenever `uci commit dynamic-pac-proxy` happens (via
`procd_add_reload_trigger`), which is what LuCI's "Save & Apply" does under
the hood. Only `listen_addr` needs a real restart, since that means
rebinding the status/PAC HTTP listener.

## Layout

```
openwrt/
  files/                     — installed verbatim onto the router's root filesystem
    etc/config/dynamic-pac-proxy        — default UCI config
    etc/init.d/dynamic-pac-proxy        — procd init script
    usr/lib/dynamic-pac-proxy/uci2yaml.sh — UCI -> config.yaml generator
    usr/lib/lua/luci/controller/...      — LuCI controller (menu + start/stop/restart + live status)
    usr/lib/lua/luci/model/cbi/...       — LuCI forms: Overview, Hosts
    usr/share/luci/menu.d/...            — LuCI menu registration
    usr/share/rpcd/acl.d/...             — LuCI/rpcd permissions
  scripts/
    build-ipk.sh             — builds a real .ipk (opkg install-able), no OpenWRT SDK needed
    install-over-ssh.sh      — pushes everything directly over SSH, no packaging step
    _mktar.py                — helper: builds root:root-owned tarballs (see comment in build-ipk.sh for why)
  README.md                  — this file
```

## UCI schema

```
config main 'main'
	option enabled '1'
	option listen_addr ':8080'
	option advertise_host ''        # blank = auto-detect, same as config.yaml
	option refresh_interval '15s'
	option mdns_timeout '2s'
	option dial_timeout '1s'
	option failure_cooldown '5s'

config host
	option name 'derek-macbook'
	option host_name 'dereks-MacBook-Pro.local'
	option host_port '8888'
	option server_port '8081'
	# optional per-host overrides, same names as above, omit to inherit:
	# option refresh_interval '5s'
	# option mdns_timeout ''
	# option dial_timeout ''
	# option failure_cooldown ''
```

`host` sections are anonymous (like `config redirect` in the firewall
config) so LuCI's table UI can add/remove them freely. `uci2yaml.sh`
silently skips any host section missing `name`/`host_name`/`host_port`/
`server_port` (logged via `logger`) rather than producing a `config.yaml`
that fails validation — the daemon's own validation is still the final
word (see the parent README's "Configuration" section for what it
enforces: unique names, port ranges, `server_port` collisions). There's no
UCI/LuCI equivalent of the daemon's `host_ip` (fixed-IP, no-mDNS) option
yet — only `host_name` is wired up here.

## Using the LuCI app

Once installed, it's under **Services → Dynamic PAC Proxy**:

- **Overview** — enable/disable, global defaults (`listen_addr`,
  `advertise_host`, timeouts, `failure_cooldown`), a running/stopped
  indicator with Start/Stop/Restart buttons, and a live-updating table of
  every host's current resolved IP/reachability/last error (polls the
  daemon's own `/status` endpoint every 5s).
- **Hosts** — add/remove/edit host entries in a table: name, mDNS
  hostname, proxy port, server port, and optional per-host overrides.

Saving either page regenerates `config.yaml` and, for anything other than
`listen_addr`, applies within a few seconds without restarting the daemon.

## Building

### Option A: a real `.ipk` (recommended for anything beyond quick testing)

```sh
# Find the router's opkg architecture first — this MUST match exactly,
# opkg refuses to install an architecture mismatch:
ssh root@<router> opkg print-architecture

# Then build (see the parent README's "OpenWRT routers" section for a
# chipset -> GOARCH table):
./scripts/build-ipk.sh -a aarch64_cortex-a53 -g arm64 -v 1.0.0

scp dist/dynamic-pac-proxy_1.0.0_aarch64_cortex-a53.ipk root@<router>:/tmp/
ssh root@<router> 'opkg install /tmp/dynamic-pac-proxy_1.0.0_aarch64_cortex-a53.ipk'
```

This is a real opkg package: `opkg remove dynamic-pac-proxy` cleanly
stops/disables the service and removes everything except your
`/etc/config/dynamic-pac-proxy` (marked as a `conffiles` entry, so opkg
preserves it). `postinst`/`prerm` enable+start / stop+disable the service
automatically.

### Option B: push directly over SSH (faster iteration, no opkg)

```sh
./scripts/install-over-ssh.sh -r 192.168.1.1 -g arm64
```

Builds the binary, copies everything into place, and restarts the
service. Won't overwrite an existing `/etc/config/dynamic-pac-proxy` on
the router (checks first) — only installs the default if none exists.
There's no corresponding uninstaller for this path; remove the files
listed under "Layout" above by hand if you want it gone.

## What's verified, what isn't

Verified in this repo without router access:
- `uci2yaml.sh`'s actual output, using a mock `functions.sh` standing in
  for OpenWRT's UCI shell library — multi-host, per-host overrides,
  zero-hosts, and skipping an incomplete host section all produce exactly
  the expected YAML, confirmed by feeding the output through the real Go
  config parser (`go test`/manual runs) — valid configs are accepted,
  empty-hosts is correctly rejected.
- Shell syntax of both init script and `uci2yaml.sh` (`sh -n`, `dash -n`).
- Lua syntax of the controller and both CBI models (`luac -p`).
- JSON validity of the ACL and menu.d files (`jq .`).
- The `.ipk`'s actual structure: exactly `debian-binary` + `control.tar.gz`
  + `data.tar.gz` in an `ar` archive (macOS's `ar` needed `-S` to stop it
  from injecting a `__.SYMDEF SORTED` member, which would have produced a
  broken package — see the comment in `build-ipk.sh`), correct
  `postinst`/`prerm`/`conffiles`/`control` content, and root:root ownership
  throughout `data.tar.gz`.

Not verified — needs a real router:
- That the LuCI pages actually render (CBI field types, `DummyValue` +
  `rawhtml` for the status/buttons widget, the `cbi/tblsection` template
  for the Hosts table) — this all follows well-established LuCI
  conventions, but "correct by convention" isn't the same as "rendered and
  clicked".
- That `opkg install` actually accepts the built `.ipk` end to end.
- `procd_add_reload_trigger` actually firing on `uci commit` for your
  OpenWRT version.

### Smoke-test checklist for first install

1. `opkg install` (or `install-over-ssh.sh`) completes without error.
2. Services → Dynamic PAC Proxy appears in the LuCI menu.
3. Overview page loads, shows RUNNING, and the host table populates.
4. Add a host on the Hosts page, save & apply — confirm `cat
   /etc/dynamic-pac-proxy/config.yaml` picked it up and `logread | grep
   dynamic-pac-proxy` shows it starting a listener on its `server_port`.
5. Click Stop / Start / Restart on Overview — confirm the status
   indicator and `/etc/init.d/dynamic-pac-proxy status` agree.
6. `opkg remove dynamic-pac-proxy` — confirm the service stops and
   `/etc/config/dynamic-pac-proxy` is left behind.

If anything in that list doesn't work as described, that's the LuCI-side
code to debug first — the daemon itself (parent directory) is exercised
by its own test suite and has been running on a real Alpine deployment
independent of any of this.
