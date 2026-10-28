//go:build windows

package session

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestIsProcessAliveWindows_OpenProcessError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "access denied", err: windows.ERROR_ACCESS_DENIED, want: true},
		{name: "invalid parameter", err: windows.ERROR_INVALID_PARAMETER, want: false},
		{name: "unknown error", err: errors.New("unexpected process probe failure"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isProcessAliveWithOpenProcess(123, func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
				require.Equal(t, uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION), access)
				require.False(t, inherit)
				require.Equal(t, uint32(123), pid)
				return 0, tt.err
			})
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsProcessAliveWindows_CurrentAndDeadProcess(t *testing.T) {
	require.True(t, isProcessAlive(os.Getpid()), "current process must be alive")
	require.False(t, isProcessAlive(0xfffffffe), "nonexistent process must be dead")
}
