// Title-normalisation tests: TestCleanTitle pins cleanTitle rules
// for deciding whether a model title response is usable.

package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCleanTitle pins the normalisation that decides whether a model's title
// response is usable: a length-truncated but non-empty response yields a
// title; a think-only or whitespace-only response yields "" (a real miss).
func TestCleanTitle(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "Tor-socks5 proxy setup", "Tor-socks5 proxy setup"},
		{"newlines collapsed", "Tor-socks5\nproxy\nsetup", "Tor-socks5 proxy setup"},
		{"trims surrounding whitespace", "   Hello world  ", "Hello world"},
		{"strips think block keeps title", "<think>pondering the ask</think>Real Title", "Real Title"},
		{"truncated title still usable", "Implement webtunnel pluggable transp", "Implement webtunnel pluggable transp"},
		{"empty stays empty", "", ""},
		{"think-only is empty", "<think>just thinking, no answer</think>", ""},
		{"orphan think tag stripped", "Title here</think>", "Title here"},
		{"whitespace only is empty", "   \n  \t", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cleanTitle(tc.raw))
		})
	}
}
