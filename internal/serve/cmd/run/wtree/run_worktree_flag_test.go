package wtree

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: catches regressions where the mirror tail drops bytes it
// must retain or keeps more than its cap.
func TestTailBuffer(t *testing.T) {
	t.Run("under cap verbatim", func(t *testing.T) {
		tb := newTailBuffer(16)
		n, err := tb.Write([]byte("hello"))
		require.NoError(t, err)
		require.Equal(t, 5, n)
		require.Equal(t, []byte("hello"), tb.Bytes())
	})
	t.Run("beyond cap keeps last bytes", func(t *testing.T) {
		tb := newTailBuffer(4)
		_, err := tb.Write([]byte("abcdefgh"))
		require.NoError(t, err)
		require.Equal(t, []byte("efgh"), tb.Bytes())
	})
	t.Run("zero length write ok", func(t *testing.T) {
		tb := newTailBuffer(4)
		n, err := tb.Write(nil)
		require.NoError(t, err)
		require.Equal(t, 0, n)
		require.Empty(t, tb.Bytes())
	})
}

// Revert-check: catches regressions where the mirror stops passing
// through to the original streams, loses the tail, or fails to restore
// os.Stdout/os.Stderr.
func TestStreamMirror(t *testing.T) {
	outOrig, err := os.CreateTemp(t.TempDir(), "out")
	require.NoError(t, err)
	defer outOrig.Close()
	errOrig, err := os.CreateTemp(t.TempDir(), "err")
	require.NoError(t, err)
	defer errOrig.Close()

	prevStdout, prevStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outOrig, errOrig
	defer func() {
		os.Stdout, os.Stderr = prevStdout, prevStderr
	}()

	m, err := StartStreamMirror()
	require.NoError(t, err)

	chunk := strings.Repeat("o", streamMirrorCapBytes+1024)
	_, err = os.Stdout.WriteString(chunk)
	require.NoError(t, err)
	_, err = os.Stderr.WriteString("stderr-marker")
	require.NoError(t, err)

	outTail, errTail, stopErr := m.Stop()
	require.NoError(t, stopErr)

	// Original files contain everything written (passthrough).
	require.NoError(t, outOrig.Sync())
	require.NoError(t, errOrig.Sync())
	passthrough, err := os.ReadFile(outOrig.Name())
	require.NoError(t, err)
	require.Len(t, passthrough, len(chunk))
	errPassthrough, err := os.ReadFile(errOrig.Name())
	require.NoError(t, err)
	require.Equal(t, "stderr-marker", string(errPassthrough))

	// Tails contain the marker and respect the cap.
	require.LessOrEqual(t, len(outTail), streamMirrorCapBytes)
	require.LessOrEqual(t, len(errTail), streamMirrorCapBytes)
	require.Contains(t, string(errTail), "stderr-marker")
	require.Equal(t, chunk[len(chunk)-streamMirrorCapBytes:], string(outTail))

	// os.Stdout/os.Stderr restored to the swapped-in files.
	require.Same(t, outOrig, os.Stdout)
	require.Same(t, errOrig, os.Stderr)
}
