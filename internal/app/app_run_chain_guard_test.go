// The reaction chain guard's CLI surface (#1113): a ChainGuard Deferred
// answer exits the loop with ONE stderr line and ONE envelope warning, no
// matter how often the scope answers again.
package app

import (
	"bytes"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// Revert-check: dropping the ChainGuard branch prints nothing (the generic
// deferred line only) and records no warning; dropping the chainNoticePrinted
// guard prints the line once per scope read (two lines here).
func TestNextStep_ChainGuardDeferredOneLineOneWarning(t *testing.T) {
	st := agent.CLIScopeState{Drain: agent.DrainDeferred, Reason: "reaction chain without progress", ChainGuard: true}
	src := &scriptedScopeSource{script: []scopeAnswer{{state: st}, {state: st}, {state: st}}}
	var stderr bytes.Buffer
	l := nextStepLoop(src)
	l.stderr = &stderr
	l.lastBuffered = new(bytes.Buffer)

	step, _, err := l.nextStep()
	require.NoError(t, err)
	require.Equal(t, stepExit, step)
	step, _, err = l.nextStep()
	require.NoError(t, err)
	require.Equal(t, stepExit, step)

	require.Equal(t, 1, bytes.Count(stderr.Bytes(), []byte("reaction chain stopped")),
		"exactly one stderr line for the whole run")
	require.Len(t, l.tot.extraWarnings, 1, "exactly one envelope warning")
	require.Contains(t, l.tot.extraWarnings[0], "reaction chain stopped")

	// A plain Deferred (not the guard) never prints the guard line.
	var plain bytes.Buffer
	l2 := nextStepLoop(&scriptedScopeSource{script: []scopeAnswer{{state: agent.CLIScopeState{
		Drain: agent.DrainDeferred, Reason: "released delegation child",
	}}}})
	l2.stderr = &plain
	_, _, err = l2.nextStep()
	require.NoError(t, err)
	require.NotContains(t, plain.String(), "reaction chain stopped")
	require.Empty(t, l2.tot.extraWarnings)

	// The guard fires while work is still open: the line prints AT THE MOMENT
	// of firing (#1113), then the loop waits on the work (Design test 6 (б)/(в)).
	var waitCase bytes.Buffer
	l3 := nextStepLoop(&scriptedScopeSource{script: []scopeAnswer{
		{state: agent.CLIScopeState{WorkOpen: true, Drain: agent.DrainDeferred, Reason: "reaction chain without progress", ChainGuard: true}},
		{state: agent.CLIScopeState{Drain: agent.DrainNone}},
	}})
	l3.stderr = &waitCase
	step, _, err = l3.nextStep()
	require.NoError(t, err)
	require.Equal(t, stepExit, step) // the script's second answer closes the scope
	require.Equal(t, 1, bytes.Count(waitCase.Bytes(), []byte("reaction chain stopped")))
	require.Len(t, l3.tot.extraWarnings, 1)
}
