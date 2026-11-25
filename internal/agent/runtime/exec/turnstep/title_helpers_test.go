package turnstep

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
			assert.Equal(t, tc.want, CleanTitle(tc.raw))
		})
	}
}

func TestTitleFromReasoning(t *testing.T) {
	tests := []struct {
		name      string
		reasoning string
		want      string
	}{
		{"final line wins", "let me think\nuser asks about proxy\nSetup Tor socks5 proxy", "Setup Tor socks5 proxy"},
		{"trailing blank lines skipped", "thinking\nThe Answer\n  \n\t\n", "The Answer"},
		{"single line", "Only Title", "Only Title"},
		{"empty", "", ""},
		{"whitespace only", "  \n \n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, TitleFromReasoning(tc.reasoning))
		})
	}
}

// TestSanitizeReasoningTitle pins the sanitisation applied to a title
// recovered from a reasoning stream: answer prefixes and wrapping quotes are
// stripped, and the line is capped at titleMaxReasoningLen (cut at a word
// boundary where possible).
func TestSanitizeReasoningTitle(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "Setup Tor socks5 proxy", "Setup Tor socks5 proxy"},
		{"title prefix stripped", `Title: "My Great Title"`, "My Great Title"},
		{"cyrillic prefix stripped", "Заголовок: Прокси через Tor", "Прокси через Tor"},
		{"dash prefix stripped", "Title - My Great Title", "My Great Title"},
		{"quotes stripped", "«Wrapped Title»", "Wrapped Title"},
		{"overlong reasoning answer rejected, not truncated", strings.Repeat("x", titleMaxReasoningLen+1), ""},
		{"multiple sentences rejected", "I cannot produce a title. The content is missing.", ""},
		{"single terminal punctuation accepted", "Fix title generation.", "Fix title generation."},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeReasoningTitle(tc.raw)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len([]rune(got)), titleMaxReasoningLen)
		})
	}
}

func TestCleanTitleRejectsModelComplaints(t *testing.T) {
	assert.Empty(t, CleanTitle("I cannot make a title. The request is empty."))
	assert.Empty(t, CleanTitle(strings.Repeat("x", titleMaxReasoningLen+1)))
}
