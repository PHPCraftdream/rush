package tools

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/platform"
)

var getRg = sync.OnceValue(func() string {
	if testing.Testing() {
		return ""
	}
	path, err := exec.LookPath("rg")
	if err != nil {
		if log.Initialized() {
			slog.Warn("Ripgrep (rg) not found in $PATH. Some grep features might be limited or slower.")
		}
		return ""
	}
	return path
})

func getRgCmd(ctx context.Context, globPattern string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	args := []string{"--files", "-L", "--null"}
	if globPattern != "" {
		if !filepath.IsAbs(globPattern) && !strings.HasPrefix(globPattern, "/") {
			globPattern = "/" + globPattern
		}
		args = append(args, "--glob", globPattern)
	}
	cmd := platform.Command(ctx, name, args...)
	return cmd
}

func getRgSearchCmd(ctx context.Context, pattern, path, include string, contextLines int) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	return buildRgSearchCmd(ctx, name, pattern, path, include, contextLines)
}

// buildRgSearchCmd assembles one rg content-search invocation. The
// command is built from an explicitly passed binary path so the
// context-aware fs_grep search can run the identical argument shape in
// tests (getRg returns "" under testing.Testing, see rg.go above).
// contextLines > 0 adds -C N: rg --json then emits "type":"context"
// messages alongside "match" messages.
func buildRgSearchCmd(ctx context.Context, rgPath, pattern, path, include string, contextLines int) *exec.Cmd {
	// Use -n to show line numbers, -0 for null separation to handle Windows paths
	args := []string{"--json", "-H", "-n", "-0", pattern}
	if contextLines > 0 {
		args = append(args, "-C", strconv.Itoa(contextLines))
	}
	if include != "" {
		args = append(args, "--glob", include)
	}
	args = append(args, path)

	return platform.Command(ctx, rgPath, args...)
}

// appendRgIgnoreFiles adds the working directory's .gitignore/.rushignore
// files to an rg invocation when they exist. Shared by every rg
// content-search caller so ignore handling cannot drift between them.
func appendRgIgnoreFiles(cmd *exec.Cmd, rootPath string) {
	// Only add ignore files if they exist.
	for _, ignoreFile := range []string{".gitignore", ".rushignore"} {
		ignorePath := filepath.Join(rootPath, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignorePath)
		}
	}
}
