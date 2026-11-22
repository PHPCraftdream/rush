package targets

// TestMain for the admin/install/targets package.
//
// The conversion implementation lives here but the //go:embed asset
// declaration lives in the parent install package, so this test binary must
// inject the canonical sources itself. The assets are read through an
// os.DirFS over the ABSOLUTE parent path resolved once here — before any
// test runs — so scope tests that t.Chdir(cwd) never invalidate the FS.
// The bytes are the same claude_wrush_command.md / claude_wcrush_command.md
// assets the parent embeds, so targets tests compare against the exact
// canonical sources.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
)

func TestMain(m *testing.M) {
	base, err := filepath.Abs("..")
	if err != nil {
		_, _ = os.Stderr.WriteString("TestMain: resolve parent dir: " + err.Error() + "\n")
		os.Exit(1)
	}
	src := os.DirFS(base)
	wrush, err := fs.ReadFile(src, "claude_wrush_command.md")
	if err != nil {
		_, _ = os.Stderr.WriteString("TestMain: read claude_wrush_command.md: " + err.Error() + "\n")
		os.Exit(1)
	}
	wcrush, err := fs.ReadFile(src, "claude_wcrush_command.md")
	if err != nil {
		_, _ = os.Stderr.WriteString("TestMain: read claude_wcrush_command.md: " + err.Error() + "\n")
		os.Exit(1)
	}
	InjectSkillSources(src, string(wrush), string(wcrush))
	cmdtest.Main(m)
}
