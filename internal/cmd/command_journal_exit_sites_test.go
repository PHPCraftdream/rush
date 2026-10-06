// REVERT-CHECK: restoring any os.Exit(n) call in a non-test internal/cmd
// file (e.g. in ping.go) must fail TestJournalNoBareOsExit.
package cmd

import (
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
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	call := regexp.MustCompile(`\bos\.Exit\(`)
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		for i, line := range strings.Split(string(data), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if call.MatchString(line) {
				offenders = append(offenders, f+":"+strconv.Itoa(i+1))
			}
		}
	}
	require.Empty(t, offenders, "use exitWithAudit instead of os.Exit")
}
