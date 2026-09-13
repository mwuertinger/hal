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
binary is at `/usr/local/bin/hal`, the config and its MQTT CA certificate are in
`/etc/hal`, and a `hal` user and group exist.

    install -m 0755 hal-arm /usr/local/bin/hal
    install -d -m 0750 -o root -g hal /etc/hal
    install -m 0640 -o root -g hal hal.yaml /etc/hal/hal.yaml
    install -m 0644 hal.service /etc/systemd/system/hal.service
    systemctl daemon-reload && systemctl enable --now hal

HAL needs no capabilities, no writable path and no devices, and the unit takes all of
them away; `systemd-analyze security hal.service` scores it 1.1 ("OK"). Two directives
are deployment-specific and must be checked before first start:

- `SocketBindAllow=tcp:8080` must match the port in `http.listen-address`.
- `IPAddressAllow=` covers loopback and RFC1918/link-local only. If the MQTT broker or
  the DNS resolver is outside the local network, add its address or HAL cannot connect.

To listen on a port below 1024, grant `CAP_NET_BIND_SERVICE` in both
`AmbientCapabilities=` and `CapabilityBoundingSet=`.

## Vendored frontend assets

The Bootstrap files under `pkg/frontend/static` diverge from upstream on purpose: the
`.map` files are not shipped and the trailing `sourceMappingURL` comments are stripped,
keeping ~963 KB of vendor debug artifacts out of the binary. Re-apply both changes when
upgrading Bootstrap.
