package config

import (
	"os"
	"testing"
)

// assertTestGlobalStateIsolated prevents tests from resolving machine-global
// paths unless their process was explicitly isolated by testguard.
func assertTestGlobalStateIsolated() {
	if testing.Testing() && os.Getenv("RUSH_TESTGUARD_ROOT") == "" {
		panic("config: global state resolved in test without testguard isolation")
	}
}
