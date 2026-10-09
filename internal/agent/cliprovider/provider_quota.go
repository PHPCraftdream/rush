package cliprovider

import (
	"encoding/json"
	"strings"
	"unicode"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/limitwords"
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

// cliLimitText keeps shared transient policy ahead of local hard keywords.
func cliLimitText(text string) (hard, transient bool) {
	class := limitwords.Classify(limitwords.Input{Status: 429, Message: text})
	if class == limitwords.Transient {
		return false, true
	}
	// Local vocabulary bridges the #1283 owner's shared-classifier work.
	// Merge this list into limitwords later; do not edit that owner's files.
	for _, word := range []string{"usage limit", "hit your limit", "hit your usage limit", "limit reached", "limit will reset", "reset at", "resets ", "try again at", "quota", "usage_limit_reached"} {
		if strings.Contains(strings.ToLower(text), word) {
			return true, false
		}
	}
	return class == limitwords.Hard, false
}

// mapCLIExitError is called only for failed exits or explicit failure events.
func mapCLIExitError(original error, stderr string, merged exitDiagnostics, failure cliFailureEvidence) error {
	var separate exitDiagnostics
	for _, line := range strings.Split(stderr, "\n") {
		separate.add([]byte(line))
	}
	diagnostics := strings.TrimSpace(failure.text + "\n" + separate.text + merged.text)
	hard, transient := cliLimitText(diagnostics)
	if transient || failure.transient || (!hard && !failure.hard) {
		return original
	}
	pe := &fantasy.ProviderError{StatusCode: 429, Title: "Quota limit", Message: diagnostics, Cause: original}
	return &cliQuotaExit{original: original, provider: pe}
}
