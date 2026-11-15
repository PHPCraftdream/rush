package cmd

// `rush logs summary` -- the parser, the counters and the two renderers. The
// fixtures are written with t.TempDir and strict LF newlines: the real writer
// emits one JSON object per line, and a CRLF or BOM in a fixture would test
// the writer of this file rather than the reader.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	logsSummaryWarnMsg = "tool call returned an error to the model"
	logsSummaryErrMsg  = "session row unreadable"
)

// logsSummaryFixture writes content to a throwaway log file and returns its
// path.
func logsSummaryFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rush.log")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// TestParseLogLine_ValidAndIrregular: one line is either a JSON object or it
// is not. A raw-text line, an empty one and a truncated one must all decode
// as "not ok" (the caller counts them as irregular), while a JSON object
// without a level is a perfectly valid entry whose level is "" -- dropping it
// would hide every line the logger ever emitted without one.
//
// Mutations made red: treating a non-object as an entry, or returning ok=true
// for a truncated line.
func TestParseLogLine_ValidAndIrregular(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantOK    bool
		wantLevel string
		wantMsg   string
		wantField string
	}{
		{
			name:      "a plain INFO line",
			line:      `{"time":"2026-09-04T09:00:56.0691487+02:00","level":"INFO","msg":"OK   20250424200609_initial.sql (1.51ms)","pid":24704}`,
			wantOK:    true,
			wantLevel: "INFO",
			wantMsg:   "OK   20250424200609_initial.sql (1.51ms)",
			wantField: "pid=24704",
		},
		{
			name:   "raw text without a leading brace",
			line:   "not json at all",
			wantOK: false,
		},
		{
			name:   "an empty line",
			line:   "",
			wantOK: false,
		},
		{
			name:   "truncated JSON",
			line:   `{"time":`,
			wantOK: false,
		},
		{
			name:      "a JSON object with no level",
			line:      `{"time":"2026-09-04T09:00:56.0691487+02:00","msg":"no level here","pid":1}`,
			wantOK:    true,
			wantLevel: "",
			wantMsg:   "no level here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry, ok := parseLogLine(tt.line)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			require.Equal(t, tt.wantLevel, entry.Level)
			require.Equal(t, tt.wantMsg, entry.Msg)
			if tt.wantField != "" {
				require.Contains(t, entry.Fields, tt.wantField)
			}
		})
	}
}

// logsSummaryCountsFixture is the fixture the counting test and the text
// renderer share: three WARN of one message, two ERROR of one message and two
// INFO -- all within a few seconds, so there is neither a process start nor a
// silence gap in it.
func logsSummaryCountsFixture(t *testing.T) string {
	t.Helper()
	return logsSummaryFixture(t, `{"time":"2026-09-04T09:00:56.0691487+02:00","level":"INFO","msg":"goose: no migrations to run","pid":24704}
{"time":"2026-09-04T09:00:57.0691487+02:00","level":"WARN","msg":"tool call returned an error to the model","pid":24704,"tool":"bash"}
{"time":"2026-09-04T09:00:57.1691487+02:00","level":"WARN","msg":"tool call returned an error to the model","pid":24704,"tool":"bash"}
{"time":"2026-09-04T09:00:57.2691487+02:00","level":"WARN","msg":"tool call returned an error to the model","pid":24704,"tool":"bash"}
{"time":"2026-09-04T09:00:58.0691487+02:00","level":"ERROR","msg":"session row unreadable","pid":24704,"session":"sess-1"}
{"time":"2026-09-04T09:00:58.1691487+02:00","level":"ERROR","msg":"session row unreadable","pid":24704,"session":"sess-2"}
{"time":"2026-09-04T09:00:59.0691487+02:00","level":"INFO","msg":"app: started run queue pump","pid":24704,"data_dir":"data/.rush"}
`)
}

// TestSummarizeFiles_CountsWarnAndErrorByMsg: the level counters, the line
// count and the (level, msg) grouping -- including the group order (count
// descending) and the fact that a group keeps the first line's fields, which
// is what tells two same-text messages apart. INFO is never grouped.
//
// Mutations made red: grouping every level instead of only WARN/ERROR
// (MsgGroups would grow), sorting by first-seen instead of by count, or
// dropping the group's fields.
func TestSummarizeFiles_CountsWarnAndErrorByMsg(t *testing.T) {
	t.Parallel()

	path := logsSummaryCountsFixture(t)
	s, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)

	require.Equal(t, 7, s.Lines)
	require.Equal(t, 3, s.Warnings)
	require.Equal(t, 2, s.Errors)

	require.Len(t, s.MsgGroups, 2)
	require.Equal(t, 3, s.MsgGroups[0].Count, "groups are ordered by count, descending")
	require.Equal(t, "WARN", s.MsgGroups[0].Level)
	require.Equal(t, logsSummaryWarnMsg, s.MsgGroups[0].Msg)
	require.Contains(t, s.MsgGroups[0].Fields, "tool=bash",
		"the group must keep the first line's fields as its example")
	require.Equal(t, 2, s.MsgGroups[1].Count)
	require.Equal(t, "ERROR", s.MsgGroups[1].Level)
	require.Equal(t, logsSummaryErrMsg, s.MsgGroups[1].Msg)
	require.Contains(t, s.MsgGroups[1].Fields, "session=sess-1")
	require.GreaterOrEqual(t, s.MsgGroups[0].Count, s.MsgGroups[1].Count)
}

// TestSummarizeFiles_CountsIrregularLines: a log that has stopped being JSON
// is the anomaly, not a reason to abort. Four raw-text lines must all be
// counted (and none of them may panic the parser), and the command still
// succeeds over the one JSON line that is there.
//
// Mutations made red: skipping non-JSON lines silently, or returning an error
// from summarizeFiles when a line fails to parse.
func TestSummarizeFiles_CountsIrregularLines(t *testing.T) {
	t.Parallel()

	path := logsSummaryFixture(t, "▶ bash ls -la\n"+
		"not json at all\n"+
		"[INFO] plain text line\n"+
		"panic: runtime error: index out of range [3] with length 3\n"+
		`{"time":"2026-09-04T09:00:56.0691487+02:00","level":"INFO","msg":"survivor","pid":1}`+"\n")

	s, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)
	require.Equal(t, 4, s.Irregular)
	require.Equal(t, 5, s.Lines)
	require.Empty(t, s.MsgGroups)
	require.Empty(t, s.Starts)
}

// logsSummaryStartsFixture is two real-shaped "process start" records: two
// rush processes sharing one log, with different version/command/ws/session
// and data_dir, in the field order internal/log/start.go emits them.
func logsSummaryStartsFixture(t *testing.T) string {
	t.Helper()
	first := time.Date(2026, 9, 4, 9, 0, 56, 0, time.UTC)
	second := first.Add(2 * time.Minute)
	return logsSummaryFixture(t, fmt.Sprintf(
		`{"time":%q,"level":"INFO","msg":"process start","pid":24704,"ppid":20001,"version":"0.2.1","command":"rush run","cwd":"proj","ws":"proj","branch":"main","session":"sess-1","data_dir":"proj/.rush"}
{"time":%q,"level":"INFO","msg":"process start","pid":31004,"ppid":20001,"version":"0.3.0-dev","command":"rush logs summary","cwd":"proj-wt","ws":"proj-wt","branch":"wcost","session":"sess-2","data_dir":"proj-wt/.rush"}
`,
		first.Format(time.RFC3339Nano), second.Format(time.RFC3339Nano)))
}

// TestSummarizeFiles_ProcessStartsInOrder: every start line is attributed to
// its version, command, workspace, session and data directory, and the
// records keep the order they appeared in.
//
// Mutations made red: reading the fields by map iteration (nondeterministic
// values), keying "ws" as "workspace", or prepending instead of appending
// (which would swap Starts[0] and Starts[1]).
func TestSummarizeFiles_ProcessStartsInOrder(t *testing.T) {
	t.Parallel()

	path := logsSummaryStartsFixture(t)
	s, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)

	require.Len(t, s.Starts, 2)

	require.Equal(t, "rush run", s.Starts[0].Command)
	require.Equal(t, "0.2.1", s.Starts[0].Version)
	require.Equal(t, "proj", s.Starts[0].Workspace)
	require.Equal(t, "sess-1", s.Starts[0].Session)
	require.Equal(t, "proj/.rush", s.Starts[0].DataDir)

	require.Equal(t, "rush logs summary", s.Starts[1].Command)
	require.Equal(t, "0.3.0-dev", s.Starts[1].Version)
	require.Equal(t, "proj-wt", s.Starts[1].Workspace)
	require.Equal(t, "sess-2", s.Starts[1].Session)
	require.Equal(t, "proj-wt/.rush", s.Starts[1].DataDir)

	require.True(t, s.Starts[0].Time.Before(s.Starts[1].Time),
		"starts keep the order they appeared in")
}

// TestSummarizeFiles_SilenceGap: only the stretch with no timestamped line at
// all counts, its bounds are the two lines around it, and the threshold is the
// --gap value the operator gave.
//
// Mutations made red: comparing against the first line instead of the previous
// one, treating any gap > 0 as silence, or ignoring --gap.
func TestSummarizeFiles_SilenceGap(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	path := logsSummaryFixture(t, fmt.Sprintf(
		`{"time":%q,"level":"INFO","msg":"app: started","pid":1}
{"time":%q,"level":"INFO","msg":"turn 1","pid":1}
{"time":%q,"level":"ERROR","msg":"boom","pid":1}
`,
		base.Format(time.RFC3339Nano),
		base.Add(5*time.Minute).Format(time.RFC3339Nano),
		base.Add(60*time.Minute).Format(time.RFC3339Nano)))

	s, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)
	require.Len(t, s.Silences, 1, "only the 55-minute stretch exceeds the 15m threshold")
	require.True(t, base.Add(5*time.Minute).Equal(s.Silences[0].From),
		"From must be the line before the gap, got %s", s.Silences[0].From)
	require.True(t, base.Add(60*time.Minute).Equal(s.Silences[0].To),
		"To must be the line after the gap, got %s", s.Silences[0].To)
	require.Equal(t, 3300.0, s.Silences[0].DurationSeconds)

	wider, err := summarizeFiles([]string{path}, 0, time.Hour)
	require.NoError(t, err)
	require.Empty(t, wider.Silences, "a 55-minute gap is quiet enough under a one-hour threshold")
}

// TestSummarizeFiles_SinceFiltersOldLines: --since drops whole lines by
// timestamp. Both lines are still read (the line counter proves it) but the
// old one must not reach the WARN counter or its message group.
//
// Mutations made red: filtering after grouping (the group would count 2), or
// not incrementing the line counter for filtered lines.
func TestSummarizeFiles_SinceFiltersOldLines(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-time.Hour)
	path := logsSummaryFixture(t, fmt.Sprintf(
		`{"time":%q,"level":"WARN","msg":%q,"pid":1,"tool":"bash"}
{"time":%q,"level":"WARN","msg":%q,"pid":1,"tool":"bash"}
`,
		old.Format(time.RFC3339Nano), logsSummaryWarnMsg,
		time.Now().Format(time.RFC3339Nano), logsSummaryWarnMsg))

	s, err := summarizeFiles([]string{path}, 30*time.Minute, defaultSilenceGap)
	require.NoError(t, err)

	require.Equal(t, 2, s.Lines, "both lines were read; one was dropped by --since")
	require.Equal(t, 1, s.Warnings)
	require.Len(t, s.MsgGroups, 1)
	require.Equal(t, 1, s.MsgGroups[0].Count,
		"the filtered-out WARN must not be counted in its group")
}

// TestSummarizeFiles_IrregularCountReachesBothRenderers: the irregular count
// is not bookkeeping, it is the answer -- both renderings must carry it, so a
// summary that silently stops counting raw-text lines is visible from either
// output an operator or a script reads.
//
// Revert-check: removing the Irregular++ in addLine leaves the counter at 0,
// which every assertion here names (struct, text header, JSON field).
func TestSummarizeFiles_IrregularCountReachesBothRenderers(t *testing.T) {
	t.Parallel()

	path := logsSummaryFixture(t, "▶ bash ls -la\n"+
		"not json at all\n"+
		"panic: runtime error: index out of range\n"+
		`{"time":"2026-09-04T09:00:56.0691487+02:00","level":"WARN","msg":"real warn","pid":1}`+"\n"+
		`{"time":"2026-09-04T09:00:57.0691487+02:00","level":"INFO","msg":"turn 1","pid":1}`+"\n")

	s, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)
	require.Equal(t, 5, s.Lines, "every non-blank line is counted, whatever it is")
	require.Equal(t, 3, s.Irregular, "three raw-text lines must be counted as irregular")
	require.Equal(t, 1, s.Warnings, "the one JSON WARN must still reach the counter")

	var text bytes.Buffer
	require.NoError(t, renderLogSummary(&text, s))
	require.Contains(t, text.String(), "IRREGULAR: 3",
		"the text render must carry the irregular count")

	var jsonBuf bytes.Buffer
	require.NoError(t, renderLogSummaryJSON(&jsonBuf, s))
	var out map[string]any
	require.NoError(t, json.Unmarshal(jsonBuf.Bytes(), &out))
	require.Equal(t, float64(3), out["irregular"],
		"the JSON render must carry the irregular count")
}

// TestSummarizeFiles_SinceKeepsFreshAndDropsStale: --since must keep the
// fresh lines and drop the stale ones, not the other way round. A line with
// no timestamp is never dropped -- there is nothing to compare it against --
// and without --since every line counts, whatever its timestamp.
//
// Revert-check: flipping the comparison to entry.Time.After(a.cutoff) keeps
// the stale lines and drops the fresh ones. The asymmetry here is what makes
// that visible: two fresh WARN against one stale, and a fresh process start
// carrying a command the stale one does not.
func TestSummarizeFiles_SinceKeepsFreshAndDropsStale(t *testing.T) {
	t.Parallel()

	stale := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	fresh := time.Now().Format(time.RFC3339Nano)
	path := logsSummaryFixture(t, fmt.Sprintf(
		`{"time":%q,"level":"WARN","msg":"stale warn","pid":1}`+"\n"+
			`{"time":%q,"level":"INFO","msg":"process start","pid":1,"command":"rush old","version":"0.1","ws":"proj","session":"old","data_dir":"proj/.rush"}`+"\n"+
			`{"time":%q,"level":"WARN","msg":"fresh warn","pid":1}`+"\n"+
			`{"time":%q,"level":"WARN","msg":"fresh warn","pid":1}`+"\n"+
			`{"time":%q,"level":"INFO","msg":"process start","pid":2,"command":"rush fresh","version":"0.2","ws":"proj","session":"new","data_dir":"proj/.rush"}`+"\n"+
			`{"level":"WARN","msg":"no timestamp warn","pid":1}`+"\n",
		stale, stale, fresh, fresh, fresh))

	// With --since: the two fresh WARN plus the timestamp-less one count, the
	// stale one does not; only the fresh process start survives.
	s, err := summarizeFiles([]string{path}, 30*time.Minute, defaultSilenceGap)
	require.NoError(t, err)
	require.Equal(t, 6, s.Lines, "every line is read; --since only drops whole entries")
	require.Equal(t, 3, s.Warnings,
		"two fresh WARN plus the timestamp-less one -- the stale one is dropped")
	require.Len(t, s.MsgGroups, 2)
	require.Len(t, s.Starts, 1)
	require.Equal(t, "rush fresh", s.Starts[0].Command,
		"the FRESH process start is the one that survives --since")

	// Without --since every line counts, whatever its timestamp.
	all, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)
	require.Equal(t, 6, all.Lines)
	require.Equal(t, 4, all.Warnings)
	require.Len(t, all.Starts, 2, "both process starts survive with no --since")
}

// TestSummarizeFiles_MissingFileIsAnError: the operator named the file, so
// skipping it would under-report -- it is an error instead.
//
// Mutations made red: swallowing the open error and returning an empty summary.
func TestSummarizeFiles_MissingFileIsAnError(t *testing.T) {
	t.Parallel()

	_, err := summarizeFiles([]string{filepath.Join(t.TempDir(), "definitely-not-here.log")}, 0, time.Minute)
	require.Error(t, err)
}

// TestRenderLogSummary_TextSections: the text names every section and prints
// "(none)" for the ones with nothing in them, so an empty section is never
// mistaken for a section that failed to run.
//
// Mutations made red: dropping the PROCESS STARTS / SILENCE GAPS headers, or
// omitting the "(none)" placeholder.
func TestRenderLogSummary_TextSections(t *testing.T) {
	t.Parallel()

	path := logsSummaryCountsFixture(t)
	s, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, renderLogSummary(&buf, s))
	out := buf.String()

	require.Contains(t, out, "FILES")
	require.Contains(t, out, path)
	require.Contains(t, out, "LINES: 7   IRREGULAR: 0   WARN: 3   ERROR: 2")
	require.Contains(t, out, "BY MESSAGE")
	require.Contains(t, out, "PROCESS STARTS")
	require.Contains(t, out, "SILENCE GAPS")
	require.Contains(t, out, logsSummaryWarnMsg)
	require.GreaterOrEqual(t, strings.Count(out, "(none)"), 2,
		"starts and silences are empty in this fixture, both must print (none)")
}

// TestRenderLogSummary_JSONCarriesEverySection: a script reads this object, so
// every section must be present under its snake_case key and a silence must
// carry its length in seconds -- nanoseconds are not a number anyone compares
// against a threshold.
//
// Mutations made red: dropping a section from the struct, renaming a JSON tag,
// or marshalling the gap as a time.Duration.
func TestRenderLogSummary_JSONCarriesEverySection(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	path := logsSummaryFixture(t, fmt.Sprintf(
		`{"time":%q,"level":"INFO","msg":"process start","pid":24704,"ppid":20001,"version":"0.2.1","command":"rush run","cwd":"proj","ws":"proj","branch":"main","session":"sess-1","data_dir":"proj/.rush"}
{"time":%q,"level":"WARN","msg":%q,"pid":24704,"tool":"bash"}
{"time":%q,"level":"ERROR","msg":"boom","pid":24704}
{"time":%q,"level":"INFO","msg":"process start","pid":31004,"ppid":20001,"version":"0.3.0-dev","command":"rush logs summary","cwd":"proj-wt","ws":"proj-wt","branch":"wcost","session":"sess-2","data_dir":"proj-wt/.rush"}
`,
		base.Format(time.RFC3339Nano),
		base.Add(10*time.Second).Format(time.RFC3339Nano), logsSummaryWarnMsg,
		base.Add(20*time.Second).Format(time.RFC3339Nano),
		base.Add(55*time.Minute+20*time.Second).Format(time.RFC3339Nano)))

	s, err := summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)
	require.Len(t, s.Starts, 2)
	require.Len(t, s.Silences, 1)

	var buf bytes.Buffer
	require.NoError(t, renderLogSummaryJSON(&buf, s))

	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	for _, key := range []string{
		"files", "lines", "irregular", "warnings", "errors",
		"msg_groups", "starts", "silences",
	} {
		require.Contains(t, out, key)
	}

	require.Equal(t, float64(1), out["warnings"])
	require.Equal(t, float64(1), out["errors"])
	require.Equal(t, float64(4), out["lines"])

	silences, ok := out["silences"].([]any)
	require.True(t, ok)
	require.Len(t, silences, 1)
	gap, ok := silences[0].(map[string]any)
	require.True(t, ok)
	require.Contains(t, gap, "from")
	require.Contains(t, gap, "to")
	seconds, ok := gap["duration_seconds"].(float64)
	require.True(t, ok, "duration_seconds must be a number")
	require.InDelta(t, 3300.0, seconds, 1.0)
}

// TestSummarizeFiles_IsReadOnly: the command's contract is that a summary
// never touches the log. Size and mtime of the fixture must be identical after
// the scan.
//
// Mutations made red: any write path added to the scan (an O_RDWR open, a
// truncate, an mtime-touching rename).
func TestSummarizeFiles_IsReadOnly(t *testing.T) {
	t.Parallel()

	path := logsSummaryCountsFixture(t)
	before, err := os.Stat(path)
	require.NoError(t, err)

	_, err = summarizeFiles([]string{path}, 0, defaultSilenceGap)
	require.NoError(t, err)

	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.Size(), after.Size(), "the log file must never change size")
	require.Equal(t, before.ModTime(), after.ModTime(), "the log file must not even be touched")
}
