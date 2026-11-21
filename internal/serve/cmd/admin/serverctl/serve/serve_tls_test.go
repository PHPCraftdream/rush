package serve

import (
	"testing"
)

func TestServeTLSFlagsAreRegistered(t *testing.T) {
	serveCmd := NewServeCmd()
	for _, name := range []string{"listen", "tls-cert", "tls-key", "client-ca", "tls-self-signed", "tls-san", "allow-cleartext", "print-config"} {
		if serveCmd.Flags().Lookup(name) == nil {
			t.Errorf("serve flag %q is not registered", name)
		}
	}
}
