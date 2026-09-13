module github.com/mwuertinger/hal

go 1.25

// Pinned so that the binary that ships is not built by whatever toolchain
// happens to be on the builder's PATH: the standard library is most of HAL's
// attack surface, and its fixes arrive as toolchain releases.
toolchain go1.26.8

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/gorilla/websocket v1.5.3
	gopkg.in/yaml.v3 v3.0.1
)

require (
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
)
