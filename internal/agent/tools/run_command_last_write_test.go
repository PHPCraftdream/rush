// REVERT-CHECK: dropping the lastWriteNS store in runCommandOutputBuffer.Write
// must fail TestRunCommandOutputBufferLastWriteAt.
package tools

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunCommandOutputBufferLastWriteAt(t *testing.T) {
	buf := newRunCommandOutputBuffer(0)
	_, ok := buf.LastWriteAt()
	require.False(t, ok, "no output yet")

	_, err := buf.Write(nil)
	require.NoError(t, err)
	_, ok = buf.LastWriteAt()
	require.False(t, ok, "an empty write is not output")

	before := time.Now()
	_, err = buf.Write([]byte("line\n"))
	require.NoError(t, err)
	at, ok := buf.LastWriteAt()
	require.True(t, ok)
	require.False(t, at.Before(before.Add(-time.Second)), "the write time is recorded")
}
