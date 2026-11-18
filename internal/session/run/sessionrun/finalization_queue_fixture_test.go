package sessionrun

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	session "github.com/PHPCraftdream/rush/internal/session/service"

	sessionpkg "github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

type finalizationQueueFixture struct {
	env        *commitHookEnv
	service    session.Service
	ctx        context.Context
	trustedCtx context.Context
	data       session.SessionAgentCallData
	raw        []byte
	identities []session.AgentFinalizationIdentity
}

func newFinalizationQueueFixture(t *testing.T) finalizationQueueFixture {
	t.Helper()
	env := finalizationAncestorEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	env.conn.SetMaxOpenConns(1)
	svc := session.NewService(db.New(env.conn), env.conn)
	parent := finalizationCommitAuthority(t, svc, "owner", "parent-call", 1)
	child := finalizationCommitAuthority(t, svc, "child", "child-call", 1)
	_, err := env.conn.ExecContext(ctx, `UPDATE batch_nodes SET token=1 WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	grandchild, err := svc.BindAgentFinalization(ctx, session.AgentFinalizationIdentity{
		SessionID: "grandchild", LogicalCallID: "queue-call", Generation: 1, NodeID: "child-root.1", NodeToken: 1,
	})
	require.NoError(t, err)
	trusted, err := session.WithAgentFinalizationContext(ctx, parent, child, grandchild)
	require.NoError(t, err)
	data := session.SessionAgentCallData{
		SessionID: "grandchild", LogicalCallID: "queue-call", Prompt: "compose settled results", AutoResumed: true,
		MaxCost: 2, MaxTokens: 100, RequestedModelRole: "reviewer",
		FinalizationRequirement: session.CaptureAgentFinalizationContext(trusted).Requirement(),
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	identities := []session.AgentFinalizationIdentity{parent.Identity, child.Identity, grandchild.Identity}
	finalizationCommitFailureDiagnostics(t, env, identities...)
	return finalizationQueueFixture{env: env, service: svc, ctx: ctx, trustedCtx: trusted, data: data, raw: raw, identities: identities}
}

func (f finalizationQueueFixture) enqueue(t *testing.T, rowID string) {
	t.Helper()
	require.NoError(t, f.service.EnqueueRunQueueEntry(f.trustedCtx, rowID, f.data.SessionID, f.raw))
}

func (f finalizationQueueFixture) lease(t *testing.T, pump string) *session.RunQueueEntry {
	t.Helper()
	entry, err := f.service.LeaseRunQueueEntry(f.ctx, f.data.SessionID, pump, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.NotNil(t, entry.FinalizationLease)
	return entry
}

func finalizationAncestorEnv(t *testing.T) *commitHookEnv {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := newCommitHookEnv(t)
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "child"))
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "grandchild"))
	require.NoError(t, env.adapter()(env.ctx, admissionSubmit("parent-root", "parent-ref", 1)))
	result, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET child_session_id='child' WHERE node_id='parent-root.1'`)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	childID := env.id
	childID.Owner = "child"
	require.NoError(t, sessionpkg.NewBatchCommitHook(env.store, childID)(env.ctx, admissionSubmit("child-root", "child-ref", 2)))
	result, err = env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET child_session_id='grandchild' WHERE node_id='child-root.1'`)
	require.NoError(t, err)
	changed, err = result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	return env
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

func commitHookCount(t *testing.T, conn *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, conn.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

type commitHookEnv struct {
	t     *testing.T
	store *sessionpkg.BatchStore
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
	return sessionpkg.NewBatchCommitHook(env.store, env.id)
}

func batchStoreTestDB(t *testing.T) (*sessionpkg.BatchStore, *sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	return sessionpkg.NewBatchStore(conn), conn, ctx
}

func seedSession(ctx context.Context, q *db.Queries, id string) error {
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
	return err
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
