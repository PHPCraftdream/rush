package queuecmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchHelp_DescribesInputAndControlBoundaries(t *testing.T) {
	_ = NewBatchCmd()
	run := oneLine(batchRunCmd.Long)
	require.Contains(t, run, "JSON or Ktav")
	require.Contains(t, run, "in this process")
	require.Contains(t, run, "no answering surface")
	require.Contains(t, run, "served intake")
	require.Contains(t, run, "There is no detached execution")
	require.Contains(t, oneLine(batchRunCmd.Flags().Lookup("spec").Usage), "JSON or Ktav")
	require.Contains(t, oneLine(batchAddCmd.Flags().Lookup("spec").Usage), "JSON or Ktav")
	require.Contains(t, oneLine(batchRunCmd.Flags().Lookup("detach").Usage), "Unsupported")

	add := oneLine(batchAddCmd.Long)
	require.Contains(t, add, "owner-process-only")
	require.Contains(t, add, "separate rush CLI process cannot access the owner's runtime")

	stop := oneLine(batchStopCmd.Long)
	require.Contains(t, stop, "By default the result is queued")
	require.Contains(t, stop, "--wait (up to 30s)")
	require.Contains(t, stop, "confirms the request was accepted, not that the node or process has terminated")
	require.Contains(t, stop, "rush batch show")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
