// REVERT-CHECK: removing the *stallDetacherTool unwrap from
// restrictedRunWrapped must fail TestRestrictedRunNotReappliedThroughStallDetach.
package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// buildTools wraps restricted -> hooks -> stall detach, and SetTools applies
// wrapToolsWithRestrictedRun again: the gate must not be stacked twice.
func TestRestrictedRunNotReappliedThroughStallDetach(t *testing.T) {
	svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
	auth := svc.(permission.RestrictedRunAuthorizer)
	restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{&dispatchProbeTool{}}, auth)
	hooked := wrapToolsWithHooks(restricted, nil, false)
	detached := wrapToolsWithStallDetach(hooked)

	again := wrapToolsWithRestrictedRun(detached, auth)
	require.Same(t, detached[0], again[0], "an already restricted tool must not get a second gate")
}
