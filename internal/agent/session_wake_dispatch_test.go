package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDispatchSessionWakeUsesCoordinatorPath(t *testing.T) {
	coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	owner, err := env.sessions.Create(context.Background(), "wake-dispatch-owner")
	require.NoError(t, err)
	require.NoError(t, store.InsertSessionNotice(context.Background(), owner.ID, "manual_test_notice", "owed", true, ""))
	coord.driverHints.claimExternalDriver(owner.ID)
	defer coord.driverHints.releaseExternalDriver(owner.ID)
	before := coord.driverHints.hintSeqOf(owner.ID)
	require.NoError(t, DispatchSessionWake(context.Background(), coord, owner.ID))
	require.Equal(t, before+1, coord.driverHints.hintSeqOf(owner.ID))
}
