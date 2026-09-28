package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunCommandOutputBuffer_ReadCursorSemantics pins wake-tools-contract.md
// §2.1's cursor rules directly against the real buffer (not a test fake):
// a valid cursor returns only bytes written since it, and a negative or
// out-of-range cursor is treated as 0, not an error.
//
// Revert-check performed: removed the `cursor < 0 || cursor > b.total`
// clamp in Read (left cursor as given) -- this test FAILED (start went
// negative and reader/no-op behavior went wrong for the negative case; the
// too-large case returned an empty slice/panicked instead of the full
// buffer). Restored the clamp; re-ran, passed.
func TestRunCommandOutputBuffer_ReadCursorSemantics(t *testing.T) {
	t.Parallel()
	buf := newRunCommandOutputBuffer(0)
	n, err := buf.Write([]byte("hello "))
	require.NoError(t, err)
	require.Equal(t, 6, n)

	data, next := buf.Read(0)
	require.Equal(t, "hello ", data)
	require.EqualValues(t, 6, next)

	_, _ = buf.Write([]byte("world"))
	data, next = buf.Read(6)
	require.Equal(t, "world", data, "must return only bytes written since the cursor")
	require.EqualValues(t, 11, next)

	// Negative cursor treated as 0.
	data, _ = buf.Read(-1)
	require.Equal(t, "hello world", data)

	// Out-of-range (beyond total) cursor treated as 0.
	data, _ = buf.Read(1000)
	require.Equal(t, "hello world", data)

	require.Equal(t, "hello world", buf.String())
}

// TestRunCommandOutputBuffer_DropsOldestBytesOnceOverCap pins the bound:
// once resident content would exceed maxBytes, the OLDEST bytes are dropped,
// writtenBytes stays monotonic, and a cursor into the dropped region is
// clamped to the earliest still-resident byte instead of panicking.
func TestRunCommandOutputBuffer_DropsOldestBytesOnceOverCap(t *testing.T) {
	t.Parallel()
	buf := newRunCommandOutputBuffer(10)
	_, _ = buf.Write([]byte("0123456789")) // exactly at cap
	require.Equal(t, "0123456789", buf.String())

	_, _ = buf.Write([]byte("AB")) // pushes cap: drops "01"
	require.Equal(t, "23456789AB", buf.String())

	// Total written bytes stays monotonic across the whole stream, not just
	// what's resident.
	_, next := buf.Read(0)
	require.EqualValues(t, 12, next)

	// A cursor into the already-dropped region ("0" or "1", offsets 0-1) is
	// clamped to the earliest resident offset (2) rather than panicking or
	// returning garbage.
	data, _ := buf.Read(0)
	require.Equal(t, "23456789AB", data)
}
