package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateFinalMCPAdmissionFileRejectsChangedSizeAndDigest(t *testing.T) {
	initial := []byte(`{"mcp":{"server":{"type":"http","url":"http://stable.example"}}}`)
	path := filepath.Join(t.TempDir(), "rush.json")
	require.NoError(t, os.WriteFile(path, initial, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)
	file, err := openStableConfigFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	require.NoError(t, validateFinalMCPAdmissionFile(context.Background(), path, file, expected, 0, false))

	t.Run("growth", func(t *testing.T) {
		mutateMCPAdmissionFile(t, path, append(initial, '!'))
		require.ErrorIs(t, validateFinalMCPAdmissionFile(context.Background(), path, file, expected, 0, false), ErrMCPMutationStale)
	})

	mutateMCPAdmissionFile(t, path, initial)

	t.Run("shrink", func(t *testing.T) {
		mutateMCPAdmissionFile(t, path, initial[:len(initial)-1])
		require.ErrorIs(t, validateFinalMCPAdmissionFile(context.Background(), path, file, expected, 0, false), ErrMCPMutationStale)
	})

	mutateMCPAdmissionFile(t, path, initial)

	t.Run("same-size digest change with restored mtime", func(t *testing.T) {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		changed := bytes.Replace(initial, []byte("stable"), []byte("change"), 1)
		require.Len(t, changed, len(initial))
		mutateMCPAdmissionFile(t, path, changed)
		require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
		require.ErrorIs(t, validateFinalMCPAdmissionFile(context.Background(), path, file, expected, 0, false), ErrMCPMutationStale)
	})
}

func TestValidateFinalMCPAdmissionFileHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	data := []byte(`{"mcp":{"server":{"type":"http","url":"http://cancel.example"}}}`)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)
	file, err := openStableConfigFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, validateFinalMCPAdmissionFile(ctx, path, file, expected, 0, false), context.Canceled)
}

func TestVerifyMCPAdmissionBytesUsesFixedReadRequests(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 3*mcpAdmissionVerifyBufferSize+17)
	reader := &recordingMCPAdmissionReader{Reader: bytes.NewReader(data)}
	require.NoError(t, verifyMCPAdmissionBytes(context.Background(), reader, int64(len(data)), sha256.Sum256(data)))
	require.NotEmpty(t, reader.requests)
	for _, size := range reader.requests {
		require.LessOrEqual(t, size, mcpAdmissionVerifyBufferSize)
	}
	require.Equal(t, 1, reader.requests[len(reader.requests)-1])
}

func TestVerifyMCPAdmissionBytesStopsAfterCancellation(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 2*mcpAdmissionVerifyBufferSize)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &recordingMCPAdmissionReader{
		Reader: bytes.NewReader(data),
		afterRead: func() {
			cancel()
		},
	}
	require.ErrorIs(t, verifyMCPAdmissionBytes(ctx, reader, int64(len(data)), sha256.Sum256(data)), context.Canceled)
}

type recordingMCPAdmissionReader struct {
	*bytes.Reader
	requests  []int
	afterRead func()
}

func (r *recordingMCPAdmissionReader) Read(p []byte) (int, error) {
	r.requests = append(r.requests, len(p))
	read, err := r.Reader.Read(p)
	if read > 0 && r.afterRead != nil {
		r.afterRead()
	}
	return read, err
}

func (r *recordingMCPAdmissionReader) Seek(offset int64, whence int) (int64, error) {
	return r.Reader.Seek(offset, whence)
}

func mutateMCPAdmissionFile(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, file.Truncate(int64(len(data))))
	_, err = file.WriteAt(data, 0)
	require.NoError(t, err)
	require.NoError(t, file.Sync())
}

var _ io.ReadSeeker = (*recordingMCPAdmissionReader)(nil)
