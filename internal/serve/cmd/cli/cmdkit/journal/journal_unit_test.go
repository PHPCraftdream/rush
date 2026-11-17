// Unit tests of the command-journal internals moved from
// internal/serve/cmd's command_journal_fallback_test.go (lane K1): they poke
// commandJournal and the allowlists directly, so
// they live in package clijournal. The e2e tests dispatching through the
// command tree stay in internal/serve/cmd; the fallback-resolver scenario
// moved back to internal/serve/cmd's command_journal_test.go because all
// its cases dispatch through the installed root.
package clijournal

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// The oracle is deliberately a literal copy, NOT derived from production
// code: widening an allowlist must take a matching edit here.
var journalOracleFlags = map[string]bool{
	"session": true, "role": true, "model": true, "effort": true,
	"timeout": true, "cwd": true, "data-dir": true, "n": true,
	"audit-days": true, "color-scheme": true, "host": true, "port": true,
	"agents": true, "max-cost": true, "max-tokens": true,
	"wait": true, "by": true, "at": true,
}

var journalOraclePositional = map[string]bool{
	"rush sessions show": true, "rush sessions last": true,
	"rush sessions tail": true, "rush sessions watch": true,
	"rush sessions kill": true, "rush sessions why": true,
	"rush sessions locks": true, "rush sessions fork": true,
	"rush sessions cancel": true, "rush sessions tree": true,
	"rush sessions cost": true, "rush sessions diff": true,
	"rush sessions reset": true, "rush sessions inject": true,
	"rush models use": true, "rush models bump": true, "rush models unset": true,
	"rush providers show": true, "rush providers set": true,
	"rush providers add": true, "rush providers update": true,
	"rush providers enable": true, "rush providers disable": true,
	"rush providers unset": true, "rush providers fetch-models": true,
	"rush providers test": true, "rush providers patch": true,
	"rush mcp show": true, "rush mcp enable": true, "rush mcp disable": true,
	"rush mcp restart": true, "rush mcp test": true, "rush mcp add": true,
	"rush mcp remove": true, "rush mcp set": true,
}

func TestJournalAllowlistsMatchOracle(t *testing.T) {
	require.Equal(t, journalOracleFlags, journalFlagValueAllowlist)
	require.Equal(t, journalOraclePositional, journalPositionalAllowlist)
}

func TestJournalErrTextFirstLineAndTruncation(t *testing.T) {
	require.Equal(t, "first line", journalErrText(errors.New("first line\nsecond line SECRET")))
	got := journalErrText(errors.New(strings.Repeat("é", 300) + "\nmore"))
	require.Equal(t, journalErrMaxRunes, utf8.RuneCountInString(got))
	require.True(t, strings.HasSuffix(got, "…"))
	require.NotContains(t, got, "more")
	require.Empty(t, journalErrText(nil))
}

func TestJournalAllowlistDenyListIntersection(t *testing.T) {
	deny := map[string]bool{
		"password": true, "api-key": true, "system-prompt": true,
		"system-prompt-file": true, "on-finish": true, "message": true,
		"prompt": true, "env": true, "header": true, "url": true,
		"arg": true, "command": true, "base-url": true, "json-body": true,
	}
	for name := range journalFlagValueAllowlist {
		require.False(t, deny[name], "allowlisted flag %q is in the hard deny list", name)
	}
	require.False(t, journalFlagValueAllowlist["password"])
	require.False(t, journalFlagValueAllowlist["json"],
		"json must stay out of the typed allowlist: providers patch --json carries a literal body")
}
