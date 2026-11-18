package session

import (
	"context"
	"database/sql"
	"testing"
	"time"

	session "github.com/PHPCraftdream/rush/internal/session/service"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

type commitHookEnv struct {
	t     *testing.T
	store *BatchStore
	conn  *sql.DB
	ctx   context.Context
	id    session.BatchCommitIdentity
}

func newCommitHookEnv(t *testing.T) *commitHookEnv {
	t.Helper()
	store, conn, ctx := batchStoreTestDB(t)
	return &commitHookEnv{
		t: t, store: store, conn: conn, ctx: ctx,
		id: session.BatchCommitIdentity{Host: "h1", Runner: "runner-a", Owner: "owner"},
	}
}

func (env *commitHookEnv) adapter() func(context.Context, jobbatch.Transition) error {
	return NewBatchCommitHook(env.store, env.id)
}

func commitHookCount(t *testing.T, conn *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, conn.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

func admissionSubmit(root, ref string, sequence uint64) jobbatch.Transition {
	return jobbatch.Transition{
		Sequence: sequence,
		Event: jobbatch.Event{
			Type: jobbatch.EventSubmit, Root: jobbatch.ID(root), Ref: ref,
			Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled"}),
		},
	}
}

func finalizationCommitService(env *commitHookEnv) session.AgentFinalizationService {
	return session.NewService(db.New(env.conn), env.conn).(session.AgentFinalizationService)
}

func finalizationCommitAuthority(t *testing.T, service session.AgentFinalizationService, owner, logical string, generation int64) *session.AgentFinalizationAuthority {
	t.Helper()
	authority, err := service.BindAgentFinalization(t.Context(), session.AgentFinalizationIdentity{SessionID: owner, LogicalCallID: logical, Generation: generation})
	require.NoError(t, err)
	return authority
}

func finalizationCommitFailureDiagnostics(t *testing.T, env *commitHookEnv, identities ...session.AgentFinalizationIdentity) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		for _, identity := range identities {
			state, err := session.NewAgentFinalizationStore(env.conn).Snapshot(ctx, identity)
			t.Logf("finalization diagnostic identity=%+v state=%+v error=%v", identity, state, err)
		}
		queries := []string{
			`SELECT session_id, logical_call_id, generation, ticket_id, released FROM session_agent_finalization_tickets ORDER BY session_id, logical_call_id, ticket_id`,
			`SELECT owner_session_id, root_id, node_id, state FROM batch_nodes ORDER BY owner_session_id, root_id, node_id`,
			`SELECT host_id, runner_id, sequence, event_type FROM batch_runner_checkpoints ORDER BY host_id, runner_id`,
		}
		for _, query := range queries {
			func() {
				rows, err := env.conn.QueryContext(ctx, query)
				if err != nil {
					t.Logf("finalization diagnostic query=%q error=%v", query, err)
					return
				}
				defer rows.Close()
				columns, err := rows.Columns()
				if err != nil {
					t.Logf("finalization diagnostic columns error=%v", err)
					return
				}
				for rows.Next() {
					values := make([]any, len(columns))
					destinations := make([]any, len(columns))
					for i := range values {
						destinations[i] = &values[i]
					}
					if err := rows.Scan(destinations...); err != nil {
						t.Logf("finalization diagnostic scan error=%v", err)
						break
					}
					t.Logf("finalization diagnostic row=%v", values)
				}
				if err := rows.Err(); err != nil {
					t.Logf("finalization diagnostic rows error=%v", err)
				}
			}()
		}
	})
}

func finalizationWriterEnv(t *testing.T) *commitHookEnv {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	return newCommitHookEnv(t)
}
