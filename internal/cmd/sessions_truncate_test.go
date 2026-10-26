package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateCyrillicNoMidRuneCut(t *testing.T) {
	in := "Запрос пустой: в сообщении пусто"
	got := truncate(in, 20)
	assert.Equal(t, "Запрос пустой: в со…", got)
	assert.True(t, utf8.ValidString(got))
	assert.LessOrEqual(t, ansi.StringWidth(got), 20)
}

func TestTruncateCJKWideRunes(t *testing.T) {
	got := truncate("世界世界世界世界", 10)
	assert.Equal(t, "世界世界…", got)
	assert.True(t, utf8.ValidString(got))
	assert.LessOrEqual(t, ansi.StringWidth(got), 10)
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

func TestTruncateShowPurposeThroughCommand(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)
	origCwd, err := os.Getwd()
	require.NoError(t, err)
	workDir := t.TempDir()
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	dataDir := filepath.Join(tmp, "show-purpose-data")
	oldDataDir := ""
	if f := sessionsShowCmd.Flags().Lookup("data-dir"); f != nil {
		oldDataDir = f.Value.String()
	}
	ensureRootFlagStandIns(sessionsShowCmd, dataDir)
	t.Cleanup(func() { _ = sessionsShowCmd.Flags().Set("data-dir", oldDataDir) })
	sessionsShowCmd.SetContext(context.Background())
	jsonFlag := sessionsShowCmd.Flags().Lookup("json")
	oldJSON := jsonFlag.Value.String()
	t.Cleanup(func() { _ = sessionsShowCmd.Flags().Set("json", oldJSON) })
	require.NoError(t, sessionsShowCmd.Flags().Set("json", "true"))
	seed, err := setupApp(sessionsShowCmd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	sess, err := seed.Sessions.CreateWithID(context.Background(), "purpose-boundary", "title")
	require.NoError(t, err)
	_, err = seed.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "a" + strings.Repeat("ё", 130)}}})
	require.NoError(t, err)
	seed.Shutdown()
	stdout := captureStdout(t, func() { require.NoError(t, sessionsShowCmd.RunE(sessionsShowCmd, []string{sess.ID})) })
	var result struct {
		Purpose string `json:"purpose"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	assert.Equal(t, "a"+strings.Repeat("ё", 118)+"…", result.Purpose)
	assert.True(t, utf8.ValidString(result.Purpose))
	assert.LessOrEqual(t, ansi.StringWidth(result.Purpose), 120)
}

func TestTruncatePreviewCJK(t *testing.T) {
	got := truncatePreview("世界世界世界", 6)
	assert.Equal(t, "世界…", got)
	assert.True(t, utf8.ValidString(got))
	assert.LessOrEqual(t, ansi.StringWidth(got), 6)
}
