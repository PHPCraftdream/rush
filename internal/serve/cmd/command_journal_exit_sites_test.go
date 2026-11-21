// REVERT-CHECK: adding an os.Exit(n) call to a production Go file anywhere
// under internal/serve/cmd (including a platform-specific file) must fail
// TestJournalNoBareOsExit and report the relative path and line.
package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A bare os.Exit skips the journal end record; exits go through exitWithAudit.
func TestJournalNoBareOsExit(t *testing.T) {
	call := regexp.MustCompile(`\bos\.Exit\(`)
	// Only the two exits in the shared TestMain adapter are exempt, once each.
	allowed := map[string]int{"os.Exit(1)": 1, "os.Exit(code)": 1}
	var offenders []string
	var scannedNested bool
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		path = filepath.ToSlash(path)
		scannedNested = scannedNested || strings.Contains(path, "/")
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !call.MatchString(line) {
				continue
			}
			if path == "cli/cmdtest/isolate.go" && allowed[trimmed] > 0 {
				allowed[trimmed]--
				continue
			}
			offenders = append(offenders, path+":"+strconv.Itoa(i+1))
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, scannedNested, "expected to scan non-test Go files in subdirectories")
	require.Empty(t, offenders, "use exitWithAudit instead of os.Exit")
}
