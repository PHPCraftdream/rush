//go:build !windows

package config

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMCPAdmissionRejectsFIFOWithoutWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	done := make(chan error, 1)
	go func() {
		_, _, err := readStableConfigFileOwned(path, 0, false)
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrConfigNonRegular)
	case <-time.After(time.Second):
		t.Fatal("FIFO admission read waited for a writer")
	}
}
