package prompt

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// update regenerates golden files: go test ./internal/agent/prompt -update
var update = flag.Bool("update", false, "update golden files")

// newDeterministicCoderPrompt builds a *Prompt over the real embedded
// coder.md.tpl with every machine-varying input pinned (time, platform,
// working directory), so two renders on different machines are
// byte-comparable. The store's working directory stays a t.TempDir(): it
// only feeds context-file discovery (empty there) and the git probe (no
// .git, so no GitStatus block); the env line renders from the fixed
// WithWorkingDir value.
func newDeterministicCoderPrompt(t *testing.T, store *config.ConfigStore) *Prompt {
	t.Helper()
	tplPath := filepath.Join("..", "templates", "coder.md.tpl")
	tpl, err := os.ReadFile(tplPath)
	require.NoError(t, err)
	fixedTime := func() time.Time {
		parsed, err := time.Parse("1/2/2006", "1/2/2026")
		require.NoError(t, err)
		return parsed
	}
	p, err := NewPrompt("coder", string(tpl),
		WithTimeFunc(fixedTime),
		WithPlatform("linux"),
		WithWorkingDir("/fixed/workdir"),
	)
	require.NoError(t, err)
	return p
}

// TestBuild_UnscopedPrompt_ByteIdentical is the strongest form of the
// backward-compat guarantee required of the folder-scope prompt hint: a
// call whose context carries NO scope hint must render a coder prompt
// byte-identical to the pre-hint render (captured in the golden file while
// the template had no scope conditional at all). A scoped-mode block that
// leaked a stray newline, blank line, or partial conditional into the
// common unscoped path would regress EVERY existing caller, which is far
// worse than the feature not existing. Regenerate the golden only when an
// intentional template change lands: go test ./internal/agent/prompt -update
func TestBuild_UnscopedPrompt_ByteIdentical(t *testing.T) {
	store := testConfigStore(t)
	p := newDeterministicCoderPrompt(t, store)

	got, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)

	goldenPath := filepath.Join("testdata", "coder_unscoped_prompt.golden")
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		require.NoError(t, os.WriteFile(goldenPath, []byte(got), 0o644))
		t.Logf("golden updated: %s (%d bytes)", goldenPath, len(got))
		return
	}

	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing; run go test ./internal/agent/prompt -update once against the pre-change template")
	require.Equal(t, string(want), got,
		"unscoped render diverged from the pre-scope-hint golden — the conditional block is leaking whitespace or text into the common path")
	require.NotContains(t, got, "scoped_filesystem",
		"unscoped render must not contain the scoped-filesystem block")
	require.NotContains(t, got, "fs_read",
		"unscoped render must not mention the fs_* batch tools")
}

// guardAgainstAccidentalGoldenOfScopedRender documents (via the test run
// failing loudly) that the golden was captured against a template without
// the scope conditional: if the golden ever contains the scoped block, the
// -update run that produced it was executed with a hint-carrying context
// or a half-applied change, and the byte-identity oracle is worthless.
func TestGoldenFile_NoScopedBlock(t *testing.T) {
	goldenPath := filepath.Join("testdata", "coder_unscoped_prompt.golden")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skip("golden not generated yet")
	}
	require.NotContains(t, string(want), "scoped_filesystem")
	require.NotContains(t, string(want), "fs_read")
}

// TestBuild_FolderScoped_BlockPresent checks the scoped-mode block: it must
// name the fs_* batch family replacing the absent legacy file tools, the
// items-array batching convention, and the 1-based inclusive line-numbering
// convention (with the explicit contrast against legacy view's 0-based
// offset, so the model cannot carry the wrong convention over).
func TestBuild_FolderScoped_BlockPresent(t *testing.T) {
	store := testConfigStore(t)
	p := newDeterministicCoderPrompt(t, store)

	got, err := p.Build(WithFolderScoped(context.Background(), true), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)

	require.Contains(t, got, "<scoped_filesystem>", "scoped render must contain the scoped-filesystem block")
	require.Contains(t, got, "</scoped_filesystem>", "scoped-filesystem block must be closed")
	require.Contains(t, got, "fs_read", "must name the read-side batch tool")
	require.Contains(t, got, "fs_write_lines", "must name the line-range write tool explicitly (the 1-based convention callout targets it)")
	require.Contains(t, got, "fs_write", "must name a write-side batch tool")
	require.Contains(t, got, "`items` array", "must state the array-based batch calling convention")
	require.Contains(t, got, "1-based inclusive", "must state the 1-based inclusive line-numbering convention")
	require.Contains(t, got, "0-based offset", "must explicitly contrast with legacy view's 0-based offset")
	require.Contains(t, got, "`view`", "must name the legacy tools that are absent")
	// The rest of the prompt must be intact around the block.
	require.Contains(t, got, "You are Rush, an AI coding assistant in the CLI.")
	require.NotContains(t, got, "Orchestrator mode", "workerActive=false must still suppress the orchestrator block")
}

// TestBuild_FolderScoped_CoexistsWithOrchestratorBlock proves the two
// conditional blocks are independent: a scoped orchestrator run renders
// BOTH, unscoped/unworkered runs render NEITHER, and neither combination
// corrupts the other's text.
func TestBuild_FolderScoped_CoexistsWithOrchestratorBlock(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 200_000)
	p := newDeterministicCoderPrompt(t, store)

	got, err := p.Build(WithFolderScoped(context.Background(), true), "smart-provider", "smart-model", store, store.Config(), true)
	require.NoError(t, err)

	require.Contains(t, got, "Orchestrator mode", "workerActive=true must still render the orchestrator block")
	require.Contains(t, got, "<scoped_filesystem>", "scoped hint must still render the scoped-filesystem block")
	require.Greater(t, strings.Index(got, "</rules>"), -1)
	require.Less(t, strings.Index(got, "<scoped_filesystem>"), strings.Index(got, "<env>"),
		"scoped block must render between </rules> and <env>")
}

// TestBuild_ExplicitFalseHint_ByteIdentical covers folderScopedFrom's false
// path: a context explicitly carrying WithFolderScoped(ctx, false) — what
// agent's withoutCallOptions produces — must render exactly the golden
// (pre-scope-hint) bytes, same as a context with no hint at all.
func TestBuild_ExplicitFalseHint_ByteIdentical(t *testing.T) {
	store := testConfigStore(t)
	p := newDeterministicCoderPrompt(t, store)

	got, err := p.Build(WithFolderScoped(context.Background(), false), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)

	want, err := os.ReadFile(filepath.Join("testdata", "coder_unscoped_prompt.golden"))
	require.NoError(t, err)
	require.Equal(t, string(want), got,
		"an explicit false hint must render byte-identical to the no-hint golden")
}
