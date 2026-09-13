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
