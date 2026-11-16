package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/diag/audit"
	"github.com/stretchr/testify/require"
)

// journalRecords returns every journalled record as decoded JSON maps.
func journalRecords(t *testing.T) []map[string]any {
	t.Helper()
	paths, err := audit.Files()
	require.NoError(t, err)
	var recs []map[string]any
	for _, p := range paths {
		bts, err := os.ReadFile(p)
		require.NoError(t, err)
		for _, line := range strings.Split(string(bts), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			recs = append(recs, rec)
		}
	}
	return recs
}

// journalReadRaw returns the raw journal bytes for leak assertions.
func journalReadRaw(t *testing.T) string {
	t.Helper()
	paths, err := audit.Files()
	require.NoError(t, err)
	var sb strings.Builder
	for _, p := range paths {
		bts, err := os.ReadFile(p)
		require.NoError(t, err)
		sb.Write(bts)
	}
	return sb.String()
}
