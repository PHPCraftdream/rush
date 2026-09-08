//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris

package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenRegularFileRejectsUnixFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	_, err := openRegularOSFile(context.Background(), path)
	require.Error(t, err)
	require.False(t, os.IsNotExist(err), "FIFO should be rejected as nonregular")
}
