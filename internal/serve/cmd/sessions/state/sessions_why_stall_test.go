package sessstate

// Revert-check documentation, one behavior per test:
//   - TestWhyStall_FreshPulseUnchanged: a fresh heartbeat on a running
//     session must NOT produce a STALLED line.
//   - TestWhyStall_StalePulseOpenTool: a >10min-stale heartbeat with a live
//     holder and an unanswered assistant tool call must print a STALLED
//     first line naming the tool and its age.
//   - TestWhyStall_StalePulseNoOpenTools: a >10min-stale heartbeat with a
//     live holder and no open tool calls must print "no open tool calls".
//   - TestWhyStall_BashArgsHidden: an open bash tool call's arguments must
//     never appear anywhere in the output ("(args hidden)" instead).

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedStallFixture builds a running session (live holder PID, held lock at
// mtimeAge old) with the given assistant tool-call parts and no results.
func seedWhyStallFixture(t *testing.T, mtimeAge time.Duration, calls ...message.ToolCall) (*app.App, string, string) {
	t.Helper()
	conn, q := cmdtest.NewSessionFactsTestDB(t)
	dataDir := t.TempDir()
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "stalled turn")
	require.NoError(t, err)

	parts := make([]message.ContentPart, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c)
	}
	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: parts,
	})
	require.NoError(t, err)

	lockPath := cmdtest.WriteLockFileAt(t, dataDir, sess.ID, os.Getpid())
	cmdtest.AgeLock(t, lockPath, mtimeAge)

	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetDataDirForTest(dataDir)
	return a, dataDir, sess.ID
}

func TestWhyStall_FreshPulseUnchanged(t *testing.T) {
	a, dataDir, id := seedWhyStallFixture(t, 0,
		message.ToolCall{ID: "call_v1", Name: "view", Input: `{"path":"x"}`})
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, id, &buf))
	require.Contains(t, buf.String(), "status: running")
	require.NotContains(t, buf.String(), "STALLED")
}

func TestWhyStall_StalePulseOpenTool(t *testing.T) {
	a, dataDir, id := seedWhyStallFixture(t, 11*time.Minute,
		message.ToolCall{ID: "call_v1", Name: "view", Input: `{"path":"x"}`})
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, id, &buf))
	lines := strings.SplitN(buf.String(), "\n", 2)
	require.True(t, strings.HasPrefix(lines[0], "STALLED "), "first line must be the STALLED header, got: %q", lines[0])
	require.Contains(t, lines[0], "<view>")
	require.Contains(t, lines[0], "call_v1")
}

func TestWhyStall_StalePulseNoOpenTools(t *testing.T) {
	a, dataDir, id := seedWhyStallFixture(t, 11*time.Minute)
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, id, &buf))
	firstLine := strings.SplitN(buf.String(), "\n", 2)[0]
	require.True(t, strings.HasPrefix(firstLine, "STALLED "), "got: %q", firstLine)
	require.Contains(t, firstLine, "no open tool calls")
}

func TestWhyStall_BashArgsHidden(t *testing.T) {
	a, dataDir, id := seedWhyStallFixture(t, 11*time.Minute,
		message.ToolCall{ID: "call_b1", Name: "bash", Input: `{"command":"rm -rf /tmp/SECRET_STUFF"}`})
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, id, &buf))
	out := buf.String()
	require.NotContains(t, out, "SECRET_STUFF")
	require.NotContains(t, out, "rm -rf")
	require.Contains(t, out, "<bash> <call_b1> open")
	require.Contains(t, out, "(args hidden)")
}

func TestTruncateStallSummaryCut(t *testing.T) {
	input := "a" + strings.Repeat("ё", 130)
	got := formatOpenToolCall(openToolCall{Name: "view", CallID: "call", Input: input})
	const prefix = "<view> <call> open 0s "
	summary, ok := strings.CutPrefix(got, prefix)
	require.True(t, ok)
	assert.Equal(t, "a"+strings.Repeat("ё", 78)+"…", summary)
	assert.NotContains(t, got, "�")
	assert.True(t, utf8.ValidString(got))
	assert.LessOrEqual(t, ansi.StringWidth(summary), 80)
}
