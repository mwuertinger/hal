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

    useradd --system --no-create-home --shell /usr/sbin/nologin hal
    install -m 0755 hal-arm /usr/local/bin/hal          # hal-amd64 on non-ARM hosts
    install -d -m 0750 -o root -g hal /etc/hal
    install -m 0640 -o root -g hal hal.yaml /etc/hal/hal.yaml
    install -m 0640 -o root -g hal ca.crt /etc/hal/ca.crt
    install -m 0644 hal.service /etc/systemd/system/hal.service
    systemctl daemon-reload && systemctl enable --now hal

See `hal.yaml.example` for the config format. `mqtt.ca-path` must point at the CA
certificate installed above.

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
  root filesystem is btrfs, xfs or f2fs, or drop the line if unsure.

Listening on a port below 1024 takes more than granting `CAP_NET_BIND_SERVICE`: a
capability held inside the unit's private user namespace does not authorize a bind
against the host's network namespace, so the bind fails with `EPERM` regardless. Prefer
a reverse proxy in front of the unprivileged port, or
`sysctl net.ipv4.ip_unprivileged_port_start=80`. To bind directly you must remove
`PrivateUsers=yes`, add `CAP_NET_BIND_SERVICE` to both `AmbientCapabilities=` and
`CapabilityBoundingSet=`, and update `SocketBindAllow=`.

## Vendored frontend assets

The Bootstrap files under `pkg/frontend/static` diverge from upstream on purpose: the
`.map` files are not shipped and the trailing `sourceMappingURL` comments are stripped,
keeping ~963 KB of vendor debug artifacts out of the binary. Re-apply both changes when
upgrading Bootstrap.
