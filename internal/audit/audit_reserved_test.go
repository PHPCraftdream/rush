package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A caller cannot set the reserved "truncated" flag: it is present only when
// Write itself cut the record. Revert-check: drop the delete in normaliseEvent => red.
func TestWriteTruncatedKeyIsReserved(t *testing.T) {
	dir := setDir(t)

	require.NoError(t, Write(Event{"kind": "short", "truncated": "yes"}))
	ints := make([]int, 2000)
	require.NoError(t, Write(Event{"kind": "big", "truncated": "no", "payload": ints}))

	files, err := Files()
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, dir, filepath.Dir(files[0]))
	recs := readRecords(t, files[0])
	require.Len(t, recs, 2)

	_, has := recs[0]["truncated"]
	require.False(t, has, "caller-supplied truncated must not survive an unshrunk record")
	require.Equal(t, true, recs[1]["truncated"])
	_, hasPayload := recs[1]["payload"]
	require.False(t, hasPayload)
}

// Large integers keep every digit. Revert-check: drop dec.UseNumber() => red.
func TestWriteKeepsLargeIntegersExact(t *testing.T) {
	setDir(t)
	require.NoError(t, Write(Event{"n": int64(9007199254740993)}))

	files, err := Files()
	require.NoError(t, err)
	b, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.True(t, strings.Contains(string(b), `"n":9007199254740993`), string(b))
}
