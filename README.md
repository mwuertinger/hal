[![Build Status](https://circleci.com/gh/mwuertinger/hal.png?style=shield&circle-token=:circle-token)](https://circleci.com/gh/mwuertinger/hal)
# HAL - Home Automation Link
Simple home automation application written in Go.

## Build

    ./build.sh

This produces self-contained static binaries `hal-amd64` and `hal-arm`. All frontend
assets (templates, CSS, JavaScript) are embedded into the binary via `go:embed`, so
the binary is the only file that needs to be deployed.

## Run

    ./hal-amd64 -config /path/to/hal.yaml

The config file and the MQTT CA certificate it points to are the only external files
required at runtime.

## Deploying with systemd

`hal.service` is a hardened unit for running HAL as a system service. It assumes the
binary is at `/usr/local/bin/hal` and that the config and its MQTT CA certificate are
in `/etc/hal`.

    cp hal.yaml.example hal.yaml && $EDITOR hal.yaml    # holds the MQTT password; gitignored
    useradd --system --no-create-home --shell /usr/sbin/nologin hal
    install -m 0755 hal-arm /usr/local/bin/hal          # hal-amd64 on non-ARM hosts
    install -d -m 0750 -o root -g hal /etc/hal
    install -m 0640 -o root -g hal hal.yaml /etc/hal/hal.yaml
    install -m 0640 -o root -g hal ca.crt /etc/hal/ca.crt
    install -m 0644 hal.service /etc/systemd/system/hal.service
    systemctl daemon-reload && systemctl enable --now hal

See `hal.yaml.example` for the config format. `mqtt.ca-path` must point at the CA
certificate installed above; the broker's certificate is verified against it, so a
self-signed broker certificate has to be its own CA here.

HAL has no authentication: being on the LAN is the credential. To keep a public
web page from using the browser of someone on that LAN as a proxy into it (DNS
rebinding), the frontend only answers requests whose `Host` is an IP address, a
single-label name such as `raspberrypi`, or a name under `.local`, `.lan`, `.home`, `.home.arpa`, `.internal`, `.localhost`, `.box` or `.localdomain`
— `hal.fritz.box` included, which is what an AVM router hands out. Reach HAL
under any other domain name and it answers 421 until that name is listed in
`http.allowed-hosts`. The authoritative list is `lanSuffixes` in
`pkg/frontend/frontend.go`.

There is no `ExecReload=`, and HAL handles only SIGINT and SIGTERM, so a config
change needs `systemctl restart hal` rather than a reload.

HAL needs no capabilities, no writable path and no devices, and the unit takes all of
them away; `systemd-analyze security hal.service` scores it 1.0 ("OK"). The residual
exposure is inherent to being a network service.

Three directives are deployment-specific and worth checking before first start:

- `SocketBindAllow=tcp:8080` must match the port in `http.listen-address`.
- `IPAddressAllow=` covers loopback and RFC1918/link-local only. The likeliest way this
  bites is DNS rather than the broker: a stock `/etc/resolv.conf` pointing at a public
  resolver such as `1.1.1.1` is outside those ranges. The symptom is an unusual
  `connect: permission denied` at startup — add the address if you see it.
- `RestrictFileSystems=` names `ext4`, the Raspberry Pi OS default. Change it if your
  root filesystem differs, or drop the line if unsure. The likely case on a Pi is
  `overlay`: raspi-config's Overlay File System option makes `/` an overlayfs, and the
  unit would then fail to start. btrfs, xfs and f2fs need the same treatment. The
  directive is inert unless `bpf` appears in `/sys/kernel/security/lsm`.

A `.local` broker name will not resolve. The binary is built with `CGO_ENABLED=0`,
and Go's own resolver does not speak mDNS even where `/etc/nsswitch.conf` lists
`mdns4_minimal` — so `ping mosquitto.local` works from a shell while HAL reports
`no such host`. Use an IP address, a name in unicast DNS, or an `/etc/hosts` entry.

Listening on a port below 1024 takes more than granting `CAP_NET_BIND_SERVICE`: a
capability held inside the unit's private user namespace does not authorize a bind
against the host's network namespace, so the bind fails with `EPERM` regardless. Prefer
a reverse proxy in front of the unprivileged port, or
`sysctl net.ipv4.ip_unprivileged_port_start=80`. To bind directly you must remove
`PrivateUsers=yes`, add `CAP_NET_BIND_SERVICE` to both `AmbientCapabilities=` and
`CapabilityBoundingSet=`, and update `SocketBindAllow=`.

## Vendored frontend assets

The frontend has no framework: `pkg/frontend/static` holds hand-written CSS and JS, so
there is nothing to upgrade there. The one vendored asset is the Inter typeface
(`static/fonts`, SIL Open Font License 1.1, license text alongside it).

Both `.woff2` files are Google Fonts' own variable-weight subsets, downloaded verbatim
from the URLs in the `@font-face` comment in `hal.css`, along with the `unicode-range`
values that go with them, over-claims and all -- Google's `latin` range names a few
codepoints the file does not actually contain, which is harmless because an absent
glyph simply falls through to the next font in the stack. Do not "fix" the ranges to
match the `cmap`: they are what makes the split work, and narrowing them would pull in
the other subset. The split is why `latin-ext` costs nothing at runtime -- a page of
German device names only ever fetches the 47 KB `latin` file.

The declared `font-weight: 400 700` is narrower than the files, whose `fvar` axis runs
100 to 900. That too is Google's own declaration, copied verbatim; it holds as long as
nothing in `hal.css` asks for a weight outside 400-700, since a request beyond the
declared range is clamped rather than synthesised.

They are served from the binary rather than from `fonts.gstatic.com` because
`hal.service` denies outbound traffic outside the LAN, so a page that reached for a CDN
would block on the font until the browser gave up on it.
