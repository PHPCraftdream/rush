//go:build windows

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestWindowsCaseSensitivityFlagParser(t *testing.T) {
	require.True(t, configWindowsCaseInsensitiveFromFlags(0))
	require.False(t, configWindowsCaseInsensitiveFromFlags(windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR))
}
