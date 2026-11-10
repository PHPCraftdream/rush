package session

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func finalizationCommitService(env *commitHookEnv) AgentFinalizationService {
	return NewService(db.New(env.conn), env.conn).(AgentFinalizationService)
}

func finalizationCommitAuthority(t *testing.T, service AgentFinalizationService, owner, logical string, generation int64) *AgentFinalizationAuthority {
	t.Helper()
	authority, err := service.BindAgentFinalization(t.Context(), AgentFinalizationIdentity{SessionID: owner, LogicalCallID: logical, Generation: generation})
	require.NoError(t, err)
	return authority
}

func finalizationCommitHook(t *testing.T, env *commitHookEnv, service AgentFinalizationService, authorities ...*AgentFinalizationAuthority) func(context.Context, jobbatch.Transition) error {
	t.Helper()
	identity, err := service.WithAgentFinalizationAuthorities(env.id, authorities...)
	require.NoError(t, err)
	return NewBatchCommitHook(env.store, identity)
}

func finalizationCommitFailureDiagnostics(t *testing.T, env *commitHookEnv, identities ...AgentFinalizationIdentity) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		for _, identity := range identities {
			state, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, identity)
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

func TestBatchCommitAuthorityRequiresExactBoundIdentity(t *testing.T) {
	for _, field := range []string{"session", "logical", "generation", "node", "token"} {
		t.Run(field, func(t *testing.T) {
			env := newCommitHookEnv(t)
			service := finalizationCommitService(env)
			if field == "session" {
				require.NoError(t, seedSession(env.ctx, db.New(env.conn), "other-owner"))
			}
			exact := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call", Generation: 1, NodeID: "r.1", NodeToken: 1}
			authority, err := service.BindAgentFinalization(t.Context(), exact)
			require.NoError(t, err)
			finalizationCommitFailureDiagnostics(t, env, exact)
			mismatch := exact
			switch field {
			case "session":
				mismatch.SessionID = "other-owner"
			case "logical":
				mismatch.LogicalCallID = "other-call"
			case "generation":
				mismatch.Generation++
			case "node":
				mismatch.NodeID = "r.2"
			case "token":
				mismatch.NodeToken++
			}
			if field == "session" {
				mismatched, err := service.BindAgentFinalization(t.Context(), mismatch)
				require.NoError(t, err)
				boundIdentity, err := service.WithAgentFinalizationAuthorities(env.id, mismatched)
				require.NoError(t, err)
				require.ErrorContains(t, NewBatchCommitHook(env.store, boundIdentity)(env.ctx, admissionSubmit("r", "call-r", 1)), "explicit authority required")
				require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
				require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
			} else {
				_, err := service.BindAgentFinalization(t.Context(), mismatch)
				require.Error(t, err, "public binding refuses every identity-component mismatch")
			}
			hook := finalizationCommitHook(t, env, service, authority)
			bound, err := service.WithAgentFinalizationAuthorities(env.id, authority)
			require.NoError(t, err)
			_, err = service.WithAgentFinalizationAuthorities(bound)
			require.ErrorContains(t, err, "already has a finalization authority binding")
			require.NoError(t, hook(env.ctx, admissionSubmit("r", "call-r", 2)))
		})
	}
}

func TestBatchCommitAuthorityNeverDowngradesKnownScope(t *testing.T) {
	for _, state := range []string{"accepted", "cancelled", "stale", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			env := newCommitHookEnv(t)
			service := finalizationCommitService(env)
			identity := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call", Generation: 1}
			if state == "stale" {
				oldIdentity := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "old-call", Generation: 1}
				oldAuthority, err := service.BindAgentFinalization(t.Context(), oldIdentity)
				require.NoError(t, err)
				_, err = env.conn.ExecContext(env.ctx, `UPDATE session_agent_finalizations SET phase='failed' WHERE session_id=? AND logical_call_id=? AND generation=?`, oldIdentity.SessionID, oldIdentity.LogicalCallID, oldIdentity.Generation)
				require.NoError(t, err)
				newIdentity := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call", Generation: 2}
				_, err = service.BindAgentFinalization(t.Context(), newIdentity)
				require.NoError(t, err)
				finalizationCommitFailureDiagnostics(t, env, oldIdentity, newIdentity)
				hook := finalizationCommitHook(t, env, service, oldAuthority)
				require.Error(t, hook(env.ctx, admissionSubmit("r", "call-r", 1)))
				record, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, newIdentity)
				require.NoError(t, err)
				require.Zero(t, record.Revision)
				require.Zero(t, record.OpenTickets)
				require.Equal(t, "executing", record.Phase)
				oldRecord, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, oldIdentity)
				require.NoError(t, err)
				require.Equal(t, "failed", oldRecord.Phase)
				require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
				require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
				return
			}
			authority, err := service.BindAgentFinalization(t.Context(), identity)
			require.NoError(t, err)
			finalizationCommitFailureDiagnostics(t, env, identity)
			switch state {
			case "accepted":
				require.NoError(t, sealFinalizationForTest(env.ctx, env.conn, NewAgentFinalizationStore(env.conn), identity, 0))
				_, err = service.BindAgentFinalization(t.Context(), identity)
				require.Error(t, err, "public binding cannot reuse an accepted identity")
			case "cancelled":
				require.NoError(t, NewService(db.New(env.conn), env.conn).RequestCancel(env.ctx, "owner"))
			case "unreadable":
				_, err = env.conn.ExecContext(env.ctx, `ALTER TABLE session_agent_finalizations RENAME TO finalizations_unreadable`)
				require.NoError(t, err)
			}
			require.Error(t, finalizationCommitHook(t, env, service)(env.ctx, admissionSubmit("r", "call-r", 1)))
			require.Error(t, finalizationCommitHook(t, env, service, authority)(env.ctx, admissionSubmit("r", "call-r", 2)))
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
			require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
		})
	}
}

func TestBatchCommitAuthorityCannotBeClearedByRewrapping(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "call", 1)
	_, err := service.WithAgentFinalizationAuthorities(env.id, authority, authority)
	require.Error(t, err)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	_, err = service.WithAgentFinalizationAuthorities(identity)
	require.Error(t, err)
	require.NoError(t, NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("r", "call-r", 1)))
}

func TestBatchCommitAuthorityRejectsForeignServiceAndMismatchedWriter(t *testing.T) {
	env := newCommitHookEnv(t)
	otherStore, otherConn, _ := batchStoreTestDB(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "call", 1)
	otherService := NewService(db.New(env.conn), env.conn).(AgentFinalizationService)
	_, err := otherService.WithAgentFinalizationAuthorities(env.id, authority)
	require.Error(t, err)
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "other-owner"))
	foreignAuthority, err := otherService.BindAgentFinalization(t.Context(), AgentFinalizationIdentity{SessionID: "other-owner", LogicalCallID: "foreign", Generation: 1})
	require.NoError(t, err)
	_, err = service.WithAgentFinalizationAuthorities(env.id, foreignAuthority)
	require.Error(t, err)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	err = NewBatchCommitHook(otherStore, identity)(env.ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "must share the service database")
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
	require.Zero(t, commitHookCount(t, otherConn, `SELECT count(*) FROM batch_nodes`))
	require.Zero(t, commitHookCount(t, otherConn, `SELECT count(*) FROM batch_runner_checkpoints`))
}

func TestBatchCommitAuthorityPreservesTrulyUnboundLegacyOwner(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	hook := finalizationCommitHook(t, env, service)
	require.NoError(t, hook(env.ctx, admissionSubmit("r", "call-r", 1)))
	owner, err := env.store.RootOwner(env.ctx, "r")
	require.NoError(t, err)
	require.Equal(t, "owner", owner)
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalizations WHERE session_id='owner'`))
}

func TestBatchCommitAuthoritySetIsRunnerWideCeilingAndOrdersOwnerTickets(t *testing.T) {
	env := newCommitHookEnv(t)
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "z-owner"))
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "x-owner"))
	service := finalizationCommitService(env)
	require.NoError(t, NewBatchCommitHook(env.store, env.id)(env.ctx, admissionSubmit("r", "call-r", 1)))
	require.NoError(t, NewBatchCommitHook(env.store, env.id)(env.ctx, admissionSubmit("s", "call-s", 2)))
	zIdentity := env.id
	zIdentity.Owner = "z-owner"
	require.NoError(t, NewBatchCommitHook(env.store, zIdentity)(env.ctx, admissionSubmit("z", "call-z", 3)))
	ownerAuthority := finalizationCommitAuthority(t, service, "owner", "owner-call", 1)
	zAuthority := finalizationCommitAuthority(t, service, "z-owner", "z-call", 1)
	untouchedAuthority := finalizationCommitAuthority(t, service, "x-owner", "untouched-call", 1)
	finalizationCommitFailureDiagnostics(t, env, ownerAuthority.identity, zAuthority.identity, untouchedAuthority.identity)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, ownerAuthority, zAuthority, untouchedAuthority)
	require.NoError(t, err)
	identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, owners []string) (func() error, error) {
		require.Equal(t, []string{"owner"}, owners)
		return func() error { return nil }, nil
	}
	// Runner checkpoints are monotonic across all owners and roots.
	require.NoError(t, NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("r", "call-r", 4)))
	require.EqualValues(t, 0, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='z-call'`),
		"a bound but unaffected runner owner is outside this transition's ticket set")
	identity.AdmitOwners = nil
	require.NoError(t, NewBatchCommitHook(env.store, identity)(env.ctx, admissionSubmit("s", "call-s", 5)))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	env.conn.SetMaxOpenConns(1)
	_, err = env.conn.ExecContext(ctx, `CREATE TRIGGER finalization_batch_requires_tickets
		BEFORE UPDATE OF state ON batch_nodes
		WHEN (SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0) <> 2
		BEGIN SELECT RAISE(ABORT, 'batch writer ran without all finalization tickets'); END`)
	require.NoError(t, err)
	identity.AdmitOwners = func(ctx context.Context, tr jobbatch.Transition, owners []string) (func() error, error) {
		require.Equal(t, jobbatch.EventStarted, tr.Event.Type)
		require.Equal(t, []string{"owner", "z-owner"}, owners)
		var open int
		require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0`).Scan(&open))
		require.Equal(t, 2, open)
		return func() error {
			var open int
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT count(*) FROM session_agent_finalization_tickets WHERE released=0`).Scan(&open))
			require.Equal(t, 2, open, "durable holds remain until callback cleanup completes")
			var running int
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE state='running'`).Scan(&running))
			require.Equal(t, 3, running)
			return nil
		}, nil
	}
	changed := []jobbatch.NodeView{
		{ID: "r", Root: "r", State: jobbatch.StateRunning, Control: jobbatch.ControlNone},
		{ID: "s", Root: "s", State: jobbatch.StateRunning, Control: jobbatch.ControlNone},
		{ID: "z", Root: "z", State: jobbatch.StateRunning, Control: jobbatch.ControlNone},
	}
	err = NewBatchCommitHook(env.store, identity)(ctx, jobbatch.Transition{
		Sequence: 6,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 1},
		Changed:  changed,
	})
	require.NoError(t, err)
	for _, authority := range []*AgentFinalizationAuthority{ownerAuthority, zAuthority} {
		record, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
		require.NoError(t, err)
		require.Zero(t, record.OpenTickets, "all tickets are released after the callback")
	}
	require.EqualValues(t, 0, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='untouched-call'`),
		"an unaffected runner capability receives no ticket")
	untouched, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, untouchedAuthority.identity)
	require.NoError(t, err)
	require.Zero(t, untouched.Revision)
	require.Zero(t, untouched.OpenTickets)
	require.EqualValues(t, 4, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE released=1`))
}

func TestBatchCommitAuthorityCancellationReleasesAdmittedTicket(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "cancel-call", 1)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	ctx, timeoutCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer timeoutCancel()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	identity.AdmitOwners = func(ctx context.Context, _ jobbatch.Transition, owners []string) (func() error, error) {
		require.Equal(t, []string{"owner"}, owners)
		require.NoError(t, NewService(db.New(env.conn), env.conn).RequestCancel(env.ctx, "owner"))
		cancel()
		return func() error { return nil }, nil
	}
	err = NewBatchCommitHook(env.store, identity)(ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorIs(t, err, context.Canceled)
	record, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 2, record.Revision)
	require.Zero(t, record.OpenTickets, "detached cleanup releases after request cancellation")
	require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='cancel-call' AND released=1`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
}

func TestBatchCommitAuthorityWriterRollbackReleasesTicket(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "rollback-call", 1)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER finalization_writer_abort
		BEFORE INSERT ON batch_nodes BEGIN SELECT RAISE(ABORT, 'finalization writer rollback'); END`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = NewBatchCommitHook(env.store, identity)(ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "finalization writer rollback")
	record, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 2, record.Revision)
	require.Zero(t, record.OpenTickets)
	require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='rollback-call' AND released=1`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
}

func TestBatchCommitAuthorityReleaseFailureLeavesCommittedProjectionHeld(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	authority := finalizationCommitAuthority(t, service, "owner", "release-fail-call", 1)
	finalizationCommitFailureDiagnostics(t, env, authority.identity)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER finalization_release_abort
		BEFORE UPDATE OF released ON session_agent_finalization_tickets
		BEGIN SELECT RAISE(ABORT, 'finalization ticket release failure'); END`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = NewBatchCommitHook(env.store, identity)(ctx, admissionSubmit("r", "call-r", 1))
	require.ErrorContains(t, err, "finalization ticket release failure")
	record, err := NewAgentFinalizationStore(env.conn).Snapshot(ctx, authority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 1, record.Revision)
	require.EqualValues(t, 1, record.OpenTickets)
	require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE node_id=root_id`), "one root node is committed")
	require.EqualValues(t, 2, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes`), "OneItem reserves root and leaf")
	require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_runner_checkpoints`))
	require.ErrorIs(t, sealFinalizationForTest(ctx, env.conn, NewAgentFinalizationStore(env.conn), authority.identity, record.Revision), ErrAgentFinalizationChanged,
		"a committed projection cannot seal while its ticket remains open")
}

func TestBatchCommitAuthorityPartialAdmissionFailureSurfacesCleanupFailure(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	require.NoError(t, NewBatchCommitHook(env.store, env.id)(env.ctx, admissionSubmit("r", "call-r", 1)))
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "z-owner"))
	zIdentity := env.id
	zIdentity.Owner = "z-owner"
	require.NoError(t, NewBatchCommitHook(env.store, zIdentity)(env.ctx, admissionSubmit("z", "call-z", 2)))
	ownerAuthority := finalizationCommitAuthority(t, service, "owner", "owner-call", 1)
	zAuthority := finalizationCommitAuthority(t, service, "z-owner", "z-call", 1)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, ownerAuthority, zAuthority)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER fail_second_finalization_admission
		BEFORE INSERT ON session_agent_finalization_tickets
		WHEN NEW.logical_call_id='z-call'
		BEGIN SELECT RAISE(ABORT, 'second owner admission failed'); END`)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER fail_partial_ticket_release
		BEFORE UPDATE OF released ON session_agent_finalization_tickets
		BEGIN SELECT RAISE(ABORT, 'partial ticket release failed'); END`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	changed := []jobbatch.NodeView{{ID: "r", Root: "r", State: jobbatch.StateRunning, Control: jobbatch.ControlNone}, {ID: "z", Root: "z", State: jobbatch.StateRunning, Control: jobbatch.ControlNone}}
	err = NewBatchCommitHook(env.store, identity)(ctx, jobbatch.Transition{
		Sequence: 5,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 1},
		Changed:  changed,
	})
	require.ErrorContains(t, err, "second owner admission failed")
	require.ErrorContains(t, err, "partial ticket release failed")
	ownerState, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, ownerAuthority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 1, ownerState.Revision)
	require.EqualValues(t, 1, ownerState.OpenTickets)
	zState, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, zAuthority.identity)
	require.NoError(t, err)
	require.Zero(t, zState.Revision)
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE state='running'`))
}

func TestBatchCommitAuthorityPartialAdmissionCleanupSucceeds(t *testing.T) {
	env := newCommitHookEnv(t)
	service := finalizationCommitService(env)
	require.NoError(t, NewBatchCommitHook(env.store, env.id)(env.ctx, admissionSubmit("r", "call-r", 1)))
	require.NoError(t, seedSession(env.ctx, db.New(env.conn), "z-owner"))
	zIdentity := env.id
	zIdentity.Owner = "z-owner"
	require.NoError(t, NewBatchCommitHook(env.store, zIdentity)(env.ctx, admissionSubmit("z", "call-z", 2)))
	ownerAuthority := finalizationCommitAuthority(t, service, "owner", "owner-call", 1)
	zAuthority := finalizationCommitAuthority(t, service, "z-owner", "z-call", 1)
	identity, err := service.WithAgentFinalizationAuthorities(env.id, ownerAuthority, zAuthority)
	require.NoError(t, err)
	_, err = env.conn.ExecContext(env.ctx, `CREATE TRIGGER fail_second_finalization_admission
		BEFORE INSERT ON session_agent_finalization_tickets
		WHEN NEW.logical_call_id='z-call'
		BEGIN SELECT RAISE(ABORT, 'second owner admission failed'); END`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	changed := []jobbatch.NodeView{{ID: "r", Root: "r", State: jobbatch.StateRunning, Control: jobbatch.ControlNone}, {ID: "z", Root: "z", State: jobbatch.StateRunning, Control: jobbatch.ControlNone}}
	err = NewBatchCommitHook(env.store, identity)(ctx, jobbatch.Transition{
		Sequence: 3,
		Event:    jobbatch.Event{Type: jobbatch.EventStarted, Node: "r.1", Token: 1},
		Changed:  changed,
	})
	require.ErrorContains(t, err, "second owner admission failed")
	ownerState, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, ownerAuthority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 2, ownerState.Revision)
	require.Zero(t, ownerState.OpenTickets)
	require.Equal(t, int64(1), commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='owner-call' AND released=1`),
		"the first owner's released receipt remains durable")
	zState, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, zAuthority.identity)
	require.NoError(t, err)
	require.Zero(t, zState.Revision)
	require.Equal(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='owner-call' AND released=1`))
	require.Zero(t, commitHookCount(t, env.conn, `SELECT count(*) FROM batch_nodes WHERE state='running'`))
}
