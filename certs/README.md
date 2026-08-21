# certs/

Drop Charles's root SSL certificate here and the daemon will list it for
download/install at `http://<advertise_host>:<listen_addr port>/certs`
(e.g. `http://172.16.2.22:8080/certs`) — the same port the status page and
`/proxy/<name>.pac` files are served from, not one of the per-host proxy
ports.

This directory is intentionally not pre-populated: Charles generates its
own root certificate per install, so there's no single certificate that
would work for everyone. Export yours from Charles and place it here.

## `dynamic-pac-proxy-ca.pem` — this tool's own certificate

If any host in `config.yaml` has `intercept_ssl: true`, this file appears
here automatically — it's generated (once, on first use) by
dynamic-pac-proxy itself, not something you export from Charles. It's the
public half of this tool's own local certificate authority ("Dynamic PAC
Proxy Local CA"), used to terminate HTTPS for hosts with `intercept_ssl`
enabled so the tool itself can decrypt and log the real request instead of
just tunneling opaque bytes — see the "SSL interception" section of the
main README.

Install/trust it the same way as a Charles certificate (same instructions
on the `/certs` page apply to both). The matching private key never lives
here — it's kept next to `config.yaml` instead, and is never served over
HTTP.

## Exporting the certificate from Charles

In Charles: **Help > SSL Proxying > Save Charles Root Certificate...**

- For iOS/macOS/Android: save as a **PEM** (`.pem`) or **binary/DER**
  (`.cer`) certificate.
- For a one-shot iOS install profile instead of a bare certificate, Charles
  can also serve `http://chls.pro/ssl` directly to a device that's already
  proxying through it — that still works independently of this page, as
  long as the device can currently reach Charles through this proxy.

Any filename works; the extension controls the `Content-Type` used when
serving it (`.pem`/`.crt`/`.cer` → `application/x-x509-ca-cert`, `.der` →
`application/pkix-cert`, `.mobileconfig` → `application/x-apple-aspen-config`),
which is what lets iOS/Android offer to install it directly instead of just
downloading a generic file.

## Where this directory lives

By default it's a `certs` folder next to `config.yaml` (so
`/opt/dynamic-pac-proxy/certs` in the current deployment — see CLAUDE.md).
Override the location with `certs_dir` in `config.yaml` if you want it
somewhere else.

Files placed here aren't committed to git (see `.gitignore`) — only this
README is tracked, so the folder still exists for a fresh checkout/build.
