// The background-job cap's refusal wording (internal/shell's counterpart to
// work_ledger_cap.go's asyncCapError): hitting MaxBackgroundJobs must name
// the next step -- free slots with job_kill, or end the turn now -- and must
// never tell the model to wait, poll or ask the user, since every running
// job already reports its result as a session message.
package shell

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBackgroundShellManager_CapMessageNamesNextStep proves the (cap+1)th
// Start's error carries the full guidance: the exact cap wording, the
// job_kill escape hatch, the "end your turn" instruction, and no "wait for"
// -- waiting is exactly the failure mode this wording exists to prevent.
func TestBackgroundShellManager_CapMessageNamesNextStep(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()
	// Pin THIS manager's cap at 1 so the assertion can quote the number in
	// the message and only one live process is needed to fill it.
	manager.SetMaxJobs(1)

	bg, err := manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
	require.NoError(t, err)
	require.NotNil(t, bg)
	require.Equal(t, 1, manager.ActiveJobs(), "the first job holds the only slot")

	_, err = manager.Start(t.Context(), workingDir, nil, "sleep 60", "")
	require.Error(t, err, "the second Start must be refused at the cap")
	require.Contains(t, err.Error(), "maximum number of background jobs (1) reached")
	require.Contains(t, err.Error(), "end your turn")
	require.Contains(t, err.Error(), "job_kill")
	require.NotContains(t, err.Error(), "wait for",
		"the cap refusal must not tell the model to wait -- nothing is queued")

	// Clean up the long-running job, as the sibling limit tests do.
	manager.KillAll(t.Context())
	require.Zero(t, manager.ActiveJobs())
}
