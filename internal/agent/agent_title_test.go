package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
		{"multiple sentences rejected", "I cannot generate a title. The request is empty.", ""},
		{"long complaint rejected", strings.Repeat("x", titleMaxReasoningLen+1), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cleanTitle(tc.raw))
		})
	}
}
