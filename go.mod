module github.com/mwuertinger/hal

go 1.25.0

// A floor, not a pin: a newer local toolchain is used as-is, and
// GOTOOLCHAIN=local ignores this line entirely. What it does guarantee is that
// nobody builds with something older, which matters because the standard
// library is most of HAL's attack surface and its fixes ship as toolchain
// releases. Note that an offline builder on an older toolchain will fail here
// rather than silently using it.
toolchain go1.26.8

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/gorilla/websocket v1.5.3
	gopkg.in/yaml.v3 v3.0.1
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
)
