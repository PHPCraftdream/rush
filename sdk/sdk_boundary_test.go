package sdk

// Phase-6 boundary guard (docs/plans/2026-08-29-embeddable-library-
// refactoring.md): the sdk production files must never perform the
// three process-level side effects that are the CLI layer's privilege —
// killing the process (os.Exit), moving the process working directory
// (os.Chdir), or hijacking the host's default logger (slog.SetDefault).
//
// The check is deliberately a textual call-pattern scan rather than an
// AST walk: this package's own documentation legitimately MENTIONS
// these names in prose ("Open never calls os.Chdir", "does not touch
// slog.Default()"), so the scanner looks for the call form "name(" and
// skips comment-only lines. It scans ONLY the .go files in this
// package's directory — transitive calls are legitimate by definition:
// Options.SetupLogging routing into internal/log (which does call
// slog.SetDefault) is exactly the sanctioned path, and the call there
// is not in a sdk file.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// forbiddenCalls are the exact call-form patterns sdk production code
// must never contain.
var forbiddenCalls = []string{
	"os.Exit(",
	"os.Chdir(",
	"slog.SetDefault(",
}

func TestSDKProductionFilesHaveNoForbiddenCalls(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed to locate this test file")
	pkgDir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(pkgDir)
	require.NoError(t, err)

	scanned := 0
	sawSdkGo := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		if name == "sdk.go" {
			sawSdkGo = true
		}
		data, err := os.ReadFile(filepath.Join(pkgDir, name))
		require.NoError(t, err)
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, call := range forbiddenCalls {
				if strings.Contains(line, call) {
					t.Errorf("%s:%d: forbidden call %s in sdk production code", name, i+1, call)
				}
			}
		}
	}

	require.Greater(t, scanned, 0, "no production .go files found in %s — the scan is vacuous", pkgDir)
	require.True(t, sawSdkGo, "sdk.go was not among the scanned files — the scan does not cover the package")
}
