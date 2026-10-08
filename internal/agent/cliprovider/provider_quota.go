package cliprovider

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"unicode"

	"charm.land/fantasy"
)

// exitDiagnostics retains only bounded, sanitized non-JSON diagnostic lines.
// JSON output may contain user, assistant or tool content, not failure evidence.
// No provider-specific failure envelopes are interpreted here.
type exitDiagnostics struct{ text string }

const maxExitDiagnostics = 8192

func (d *exitDiagnostics) add(line []byte) {
	line = ansiEscape.ReplaceAll(line, nil)
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 || json.Valid(line) || line[0] == '{' || line[0] == '[' {
		return
	}
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' {
			return -1
		}
		return r
	}, string(line))
	remaining := maxExitDiagnostics - len(d.text)
	if remaining <= 1 {
		return
	}
	if len(clean) > remaining-1 {
		clean = clean[:remaining-1]
	}
	d.text += clean + "\n"
}

// cliQuotaExit preserves the original process error's text and exposes the
// structured provider error through Unwrap, including its original exit cause.
type cliQuotaExit struct {
	original error
	provider *fantasy.ProviderError
}

func (e *cliQuotaExit) Error() string {
	text := e.original.Error()
	if !strings.Contains(text, e.provider.Message) {
		text += "\ndiagnostics: " + e.provider.Message
	}
	return text
}
func (e *cliQuotaExit) Unwrap() []error { return []error{e.provider, e.original} }

// mapCLIExitError uses only the existing generic HTTP-policy quota vocabulary.
// It must be called only for failed exits, never successful output or events.
func mapCLIExitError(original error, stderr string, merged exitDiagnostics) error {
	var exit *exec.ExitError
	if !errors.As(original, &exit) {
		return original
	}
	var separate exitDiagnostics
	for _, line := range strings.Split(stderr, "\n") {
		separate.add([]byte(line))
	}
	diagnostics := strings.TrimSpace(separate.text + merged.text)
	lower := strings.ToLower(diagnostics)
	if !strings.Contains(lower, "usage limit") && !strings.Contains(lower, "limit will reset") && !strings.Contains(lower, "reset at") && !strings.Contains(lower, "quota") {
		return original
	}
	pe := &fantasy.ProviderError{StatusCode: 429, Title: "Quota limit", Message: diagnostics, Cause: original}
	return &cliQuotaExit{original: original, provider: pe}
}
