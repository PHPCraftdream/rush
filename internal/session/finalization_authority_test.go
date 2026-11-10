package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestWithAgentFinalizationAuthoritiesRejectsMalformedAndConflictingSets(t *testing.T) {
	env := newCommitHookEnv(t)
	api := finalizationCommitService(env)
	identity := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "call", Generation: 1}
	first, err := api.BindAgentFinalization(env.ctx, identity)
	require.NoError(t, err)
	_, err = api.WithAgentFinalizationAuthorities(env.id, nil)
	require.Error(t, err)
	_, err = api.WithAgentFinalizationAuthorities(env.id, first, first)
	require.Error(t, err)
	require.NoError(t, sealFinalizationForTest(env.ctx, env.conn, NewAgentFinalizationStore(env.conn), identity, 0))
	second, err := api.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{
		SessionID: identity.SessionID, LogicalCallID: identity.LogicalCallID, Generation: 2,
	})
	require.NoError(t, err)
	_, err = api.WithAgentFinalizationAuthorities(env.id, first, second)
	require.ErrorContains(t, err, "conflicting finalization authorities")
	_, malformedErr := api.WithAgentFinalizationAuthorities(env.id, &AgentFinalizationAuthority{
		service: first.service, identity: AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "", Generation: 1},
	})
	require.Error(t, malformedErr)
}

func TestAgentFinalizationTicketReleaseIsIdempotentAcrossRepeatedCalls(t *testing.T) {
	env := newCommitHookEnv(t)
	identity := AgentFinalizationIdentity{SessionID: "owner", LogicalCallID: "release-twice", Generation: 1}
	store := NewAgentFinalizationStore(env.conn)
	require.NoError(t, store.Bind(env.ctx, identity))
	admitted, err := store.Admit(env.ctx, identity, "same-ticket")
	require.NoError(t, err)
	require.EqualValues(t, 1, admitted.Revision)
	require.NoError(t, store.Release(env.ctx, identity, "same-ticket"))
	afterFirst, err := store.Snapshot(env.ctx, identity)
	require.NoError(t, err)
	require.EqualValues(t, 2, afterFirst.Revision)
	require.Zero(t, afterFirst.OpenTickets)
	require.NoError(t, store.Release(env.ctx, identity, "same-ticket"))
	afterSecond, err := store.Snapshot(env.ctx, identity)
	require.NoError(t, err)
	require.Equal(t, afterFirst, afterSecond, "repeated release does not advance revision or lower the hold again")
}

func TestBatchCommitOwnerAdmissionReleaseClosureIsIdempotent(t *testing.T) {
	env := newCommitHookEnv(t)
	api := finalizationCommitService(env)
	authority, err := api.BindAgentFinalization(env.ctx, AgentFinalizationIdentity{
		SessionID: "owner", LogicalCallID: "closure-release", Generation: 1,
	})
	require.NoError(t, err)
	identity, err := api.WithAgentFinalizationAuthorities(env.id, authority)
	require.NoError(t, err)
	callbackReleases := 0
	identity.AdmitOwners = func(context.Context, jobbatch.Transition, []string) (func() error, error) {
		return func() error {
			callbackReleases++
			return nil
		}, nil
	}
	release, err := admitBatchCommitOwners(env.store, env.ctx, identity, admissionSubmit("r", "call-r", 1), []string{"owner", "owner"})
	require.NoError(t, err)
	require.NotNil(t, release)
	before, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 1, before.Revision)
	require.EqualValues(t, 1, before.OpenTickets)
	require.EqualValues(t, 1, commitHookCount(t, env.conn, `SELECT count(*) FROM session_agent_finalization_tickets WHERE logical_call_id='closure-release'`))
	require.NoError(t, release())
	afterFirst, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
	require.NoError(t, err)
	require.EqualValues(t, 2, afterFirst.Revision)
	require.Zero(t, afterFirst.OpenTickets)
	require.NoError(t, release())
	afterSecond, err := NewAgentFinalizationStore(env.conn).Snapshot(env.ctx, authority.identity)
	require.NoError(t, err)
	require.Equal(t, afterFirst, afterSecond)
	require.Equal(t, 1, callbackReleases)
}
