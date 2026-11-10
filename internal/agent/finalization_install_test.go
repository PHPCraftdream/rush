package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchInstallEnforcesPersistedFinalizationAuthority(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_capability", true: "trusted_capability"}[trusted], func(t *testing.T) {
			t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
			t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
			store, conn, ctx := newTestBatchStore(t)
			_, err := db.New(conn).CreateSession(ctx, db.CreateSessionParams{ID: "owner", Title: "owner"})
			require.NoError(t, err)
			sessions := session.NewService(db.New(conn), conn)
			deps := installTestDeps(t, store, conn, "unused")
			deps.Sessions = sessions
			install, err := InstallBatchRuntimeDeferred(ctx, deps)
			require.NoError(t, err)
			identity := session.AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call", Generation: 1}
			authority, err := sessions.(session.AgentFinalizationService).BindAgentFinalization(ctx, identity)
			require.NoError(t, err)
			if trusted {
				ctx, err = session.WithAgentFinalizationContext(ctx, authority)
				require.NoError(t, err)
			}
			err = install.CommitHook(ctx, jobbatch.Transition{
				Sequence: 1,
				Event: jobbatch.Event{
					Type: jobbatch.EventSubmit, Root: "r", Ref: "ref",
					Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}),
				},
			})
			var nodes int64
			readErr := conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes`).Scan(&nodes)
			require.NoError(t, readErr)
			state, readErr := session.NewAgentFinalizationStore(conn).Snapshot(ctx, identity)
			require.NoError(t, readErr)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("trusted=%t nodes=%d state=%+v commit_error=%v", trusted, nodes, state, err)
				}
			})
			if trusted {
				require.NoError(t, err)
				require.EqualValues(t, 2, nodes)
				require.EqualValues(t, 2, state.Revision)
			} else {
				require.ErrorContains(t, err, "explicit authority required")
				require.Zero(t, nodes)
				require.Zero(t, state.Revision)
			}
			require.Zero(t, state.OpenTickets)
		})
	}
}
