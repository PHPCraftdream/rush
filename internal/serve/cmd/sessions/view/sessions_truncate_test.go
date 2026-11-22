package sessview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateCyrillicNoMidRuneCut(t *testing.T) {
	in := "Запрос пустой: в сообщении пусто"
	got := cliout.Truncate(in, 20)
	assert.Equal(t, "Запрос пустой: в со…", got)
	assert.True(t, utf8.ValidString(got))
	assert.LessOrEqual(t, ansi.StringWidth(got), 20)
}

func TestTruncateCJKWideRunes(t *testing.T) {
	got := cliout.Truncate("世界世界世界世界", 10)
	assert.Equal(t, "世界世界…", got)
	assert.True(t, utf8.ValidString(got))
	assert.LessOrEqual(t, ansi.StringWidth(got), 10)
}

func TestTruncateShowPurposeThroughCommand(t *testing.T) {
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	origCwd, err := os.Getwd()
	require.NoError(t, err)
	workDir := t.TempDir()
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	dataDir := filepath.Join(tmp, "show-purpose-data")
	cmd := NewSessionsShowCmd(cmdkit.SetupApp)
	ensureShowRootFlagStandIns(cmd, dataDir)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Flags().Set("json", "true"))
	seed, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	sess, err := seed.Sessions.CreateWithID(context.Background(), "purpose-boundary", "title")
	require.NoError(t, err)
	_, err = seed.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "a" + strings.Repeat("ё", 130)}}})
	require.NoError(t, err)
	seed.Shutdown()
	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, cmd.RunE(cmd, []string{sess.ID}))
	})
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
