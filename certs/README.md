# certs/

Drop Charles's root SSL certificate here and the daemon will list it for
download/install at `http://<advertise_host>:<listen_addr port>/certs`
(e.g. `http://172.16.2.22:8080/certs`) — the same port the status page and
`/proxy/<name>.pac` files are served from, not one of the per-host proxy
ports.

This directory is intentionally not pre-populated: Charles generates its
own root certificate per install, so there's no single certificate that
would work for everyone. Export yours from Charles and place it here.

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
