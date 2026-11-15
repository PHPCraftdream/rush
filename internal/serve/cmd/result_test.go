// Fork-only tests for the `rush result` command (result.go).
// Revert-check comments state which product change each test catches.

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cyrEnv     = `{"session_id":"s1-review","exit_reason":"end_turn","final_text":"Проверено: замечаний нет.","review":"Замечаний нет, изменения точны.","review_verdict":"pass","usage":{"delta_tokens":120,"delta_cost_usd":0.02},"duration_ms":65000}`
	warnEnv    = `{"session_id":"s2","exit_reason":"stop","final_text":"","warnings":["final_text is empty and no tools were called this turn.","reduction-loss: final_text is 0 chars."]}`
	warnErrEnv = `{"session_id":"s2","exit_reason":"stop","final_text":"","error":"Provider X stopped sending streaming data","warnings":["final_text is empty and no tools were called this turn."]}`
	minEnv     = `{"exit_reason":"stop"}`
)

// newResultHarness builds a fresh result command sharing resultCmd's
// RunE and flag set, so flag state never leaks between tests that may
// run concurrently.
func newResultHarness(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "result", RunE: resultCmd.RunE, SilenceUsage: true}
	addResultFlags(cmd)
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	return cmd, &buf
}

func writeEnv(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// Revert-check: any future encoding conversion / codepage-aware writer
// would corrupt Cyrillic; the raw-passthrough guarantee is what the
// acceptance criterion "no failures at any console codepage" rests on.
func TestResultTextCyrillic(t *testing.T) {
	cmd, buf := newResultHarness(t)
	path := writeEnv(t, "cyr.out", cyrEnv)
	cmd.SetArgs([]string{path})
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "Проверено: замечаний нет.")
	assert.True(t, utf8.Valid(buf.Bytes()))
	assert.Contains(t, out, "exit reason: end_turn")
	assert.Contains(t, out, "usage: 120 tokens")
	assert.Contains(t, out, "duration: 1m5s")
}

// Revert-check: text mode must not hide warnings/error when final_text
// is empty.
func TestResultTextEmptyFinalTextWithWarnings(t *testing.T) {
	cmd, buf := newResultHarness(t)
	path := writeEnv(t, "warn.out", warnErrEnv)
	cmd.SetArgs([]string{path})
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "final text:")
	assert.Contains(t, out, "(empty)")
	assert.Contains(t, out, "- final_text is empty and no tools were called this turn.")
	assert.Contains(t, out, "error: Provider X stopped sending streaming data")

	cmd2, buf2 := newResultHarness(t)
	path2 := writeEnv(t, "warn2.out", warnEnv)
	cmd2.SetArgs([]string{path2})
	require.NoError(t, cmd2.Execute())
	assert.Contains(t, buf2.String(), "- reduction-loss: final_text is 0 chars.")
}

// Revert-check: an envelope with omitted fields must render, not panic.
func TestResultTextMinimal(t *testing.T) {
	cmd, buf := newResultHarness(t)
	path := writeEnv(t, "min.out", minEnv)
	cmd.SetArgs([]string{path})
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "exit reason: stop")
	assert.Contains(t, out, "(empty)")
	assert.NotContains(t, out, "warnings:")
}

// Revert-check: --json must not re-marshal/reorder/re-encode the
// envelope; scripts diff the bytes.
func TestResultJSONPassthrough(t *testing.T) {
	cmd, buf := newResultHarness(t)
	// No trailing newline on purpose: the bytes must be written back
	// exactly as stored.
	path := writeEnv(t, "raw.out", cyrEnv)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.False(t, bytes.HasSuffix(raw, []byte("\n")))
	cmd.SetArgs([]string{path, "--json"})
	require.NoError(t, cmd.Execute())
	assert.True(t, bytes.Equal(buf.Bytes(), raw))
}

// Revert-check: --review mode scoping — only verdict and review text,
// never the executor's final_text or the text-mode labels.
func TestResultReview(t *testing.T) {
	cmd, buf := newResultHarness(t)
	path := writeEnv(t, "cyr.out", cyrEnv)
	cmd.SetArgs([]string{path, "--review"})
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "verdict: pass")
	assert.Contains(t, out, "Замечаний нет, изменения точны.")
	assert.NotContains(t, out, "Проверено")
	assert.NotContains(t, out, "exit reason")
}

// Revert-check: an envelope without a review prints nothing and still
// exits 0 (nil error).
func TestResultReviewEmpty(t *testing.T) {
	cmd, buf := newResultHarness(t)
	path := writeEnv(t, "min.out", minEnv)
	cmd.SetArgs([]string{path, "--review"})
	require.NoError(t, cmd.Execute())
	assert.Empty(t, buf.String())
}

// Revert-check: raw-string contract for scripts — the field value is
// written without quotes, plus exactly one trailing newline.
func TestResultFieldString(t *testing.T) {
	cmd, buf := newResultHarness(t)
	path := writeEnv(t, "cyr.out", cyrEnv)
	cmd.SetArgs([]string{path, "--field", "final_text"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "Проверено: замечаний нет.\n", buf.String())
}

// Revert-check: non-string fields render as JSON, not Go syntax.
func TestResultFieldNonString(t *testing.T) {
	cmd, buf := newResultHarness(t)
	path := writeEnv(t, "cyr.out", cyrEnv)
	cmd.SetArgs([]string{path, "--field", "usage"})
	require.NoError(t, cmd.Execute())
	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Contains(t, buf.String(), "delta_tokens")

	cmd2, buf2 := newResultHarness(t)
	path2 := writeEnv(t, "warn.out", warnEnv)
	cmd2.SetArgs([]string{path2, "--field", "warnings"})
	require.NoError(t, cmd2.Execute())
	var arr []any
	require.NoError(t, json.Unmarshal(buf2.Bytes(), &arr))
	assert.Len(t, arr, 2)
}

// Revert-check: an absent field must fail loudly, not silently print
// an empty line.
func TestResultFieldMissing(t *testing.T) {
	cmd, _ := newResultHarness(t)
	path := writeEnv(t, "cyr.out", cyrEnv)
	cmd.SetArgs([]string{path, "--field", "nope"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"nope"`)
	assert.Contains(t, err.Error(), "not found in envelope")
}

// Revert-check: a missing envelope must exit non-zero, never render
// garbage.
func TestResultMissingEnvelope(t *testing.T) {
	cmd, _ := newResultHarness(t)
	cmd.SetArgs([]string{filepath.Join(t.TempDir(), "absent.out")})
	assert.Error(t, cmd.Execute())
}

// Revert-check: --json, --review and --field are mutually exclusive;
// the conflict must error instead of picking one silently.
func TestResultConflictingModes(t *testing.T) {
	cmd, _ := newResultHarness(t)
	path := writeEnv(t, "cyr.out", cyrEnv)
	cmd.SetArgs([]string{path, "--json", "--review"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// Revert-check: guards accidental flag renames that would break
// scripts relying on --json/--review/--field.
func TestResultMutualFlagHelper(t *testing.T) {
	cmd := &cobra.Command{Use: "result"}
	addResultFlags(cmd)
	names := make(map[string]bool)
	cmd.Flags().VisitAll(func(f *pflag.Flag) { names[f.Name] = true })
	assert.Len(t, names, 3)
	assert.True(t, names["json"])
	assert.True(t, names["review"])
	assert.True(t, names["field"])
}
