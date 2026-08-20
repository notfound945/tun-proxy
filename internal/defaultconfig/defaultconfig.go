// Package defaultconfig provides the configuration template compiled into the
// tun-proxy binary.
package defaultconfig

import _ "embed"

//go:embed config.yaml
var contents []byte

// Bytes returns a private copy of the embedded default configuration.
func Bytes() []byte {
	return append([]byte(nil), contents...)
}
