package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeakHoursCommandsKeepExistingAdminAuthority(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"set_provider_peak_hours",
		"pause_provider_peak_hours",
		"resume_provider_peak_hours",
		"get_peak_hours_status",
	} {
		require.Equal(t, "admin", commandScopes[command], command)
	}
}
