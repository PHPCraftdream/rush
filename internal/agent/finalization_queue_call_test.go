package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFinalizationCallDataIsDeclarationNotCapability(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	call := SessionAgentCall{
		SessionID: fixture.child.SessionID, LogicalCallID: fixture.child.LogicalCallID,
		Prompt: "compose settled results", MaxCost: 2, MaxTokens: 100,
		RequestedModelRole: RequestedModelRole("reviewer"), finalizationAuthorities: fixture.capability,
	}
	data := ToSessionAgentCallData(call)
	require.Equal(t, fixture.capability.Requirement(), data.FinalizationRequirement)
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, data, decoded)
	_, err = FromSessionAgentCallData(decoded)
	require.ErrorIs(t, err, session.ErrAgentFinalizationChanged, "generic decoding cannot reconstruct authority from payload IDs")
	decoded.FinalizationRequirement = nil
	legacy, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	require.Nil(t, legacy.finalizationAuthorities.Requirement())
	require.Equal(t, call.MaxCost, legacy.MaxCost)
	require.Equal(t, call.MaxTokens, legacy.MaxTokens)
	require.Equal(t, call.RequestedModelRole, legacy.RequestedModelRole)
}

func TestQueuedFinalizationRefusesBeforeModelConstructionWithoutLeaseProof(t *testing.T) {
	for _, declaration := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmarked_bound_scope", true: "forged_declaration"}[declaration], func(t *testing.T) {
			fixture := newFinalizationDriverFixture(t)
			data := session.SessionAgentCallData{SessionID: fixture.child.SessionID, LogicalCallID: fixture.child.LogicalCallID, Prompt: "must not dispatch"}
			if declaration {
				data.FinalizationRequirement = fixture.capability.Requirement()
			}
			// No config/model/provider exists: rejection must precede all rebuilds.
			coordinator := &coordinator{sessions: session.NewService(db.New(fixture.conn), fixture.conn)}
			_, err := coordinator.RebuildSessionAgentCall(session.WithRunQueueRowID(context.Background(), "forged-row"), "forged-row", data)
			require.ErrorIs(t, err, session.ErrAgentFinalizationChanged)
			var attempted *ErrCallAlreadyAttempted
			require.ErrorAs(t, err, &attempted)
			var receipts int64
			require.NoError(t, fixture.conn.QueryRowContext(fixture.ctx, `SELECT count(*) FROM session_agent_finalization_continuations`).Scan(&receipts))
			require.Zero(t, receipts)
		})
	}
}

func TestFinalizationChildQueueCannotSubstituteRootDriver(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	root := &sessionAgent{}
	child := &sessionAgent{}
	coordinator := &coordinator{sessions: session.NewService(db.New(fixture.conn), fixture.conn), currentAgent: root, subAgentDrivers: newSubAgentDriverRegistry()}
	call := SessionAgentCall{SessionID: fixture.child.SessionID, LogicalCallID: fixture.child.LogicalCallID, finalizationAuthorities: fixture.capability}
	_, err := coordinator.RunSessionAgentCall(fixture.ctx, call)
	require.ErrorIs(t, err, session.ErrAgentFinalizationDriverUnavailable, "restart cannot invent a child driver or execute on the root")
	var attempted *ErrCallAlreadyAttempted
	require.ErrorAs(t, err, &attempted)
	coordinator.subAgentDrivers.register(call.SessionID, subAgentDriver{agent: child, call: SessionAgentCall{SessionID: call.SessionID, LogicalCallID: "replacement"}, finalizationAuthorities: fixture.capability})
	_, err = coordinator.finalizationQueueDriver(call)
	require.ErrorIs(t, err, session.ErrAgentFinalizationDriverUnavailable, "a recorded but different logical driver is not proof")
	coordinator.subAgentDrivers.register(call.SessionID, subAgentDriver{agent: child, call: call, finalizationAuthorities: fixture.capability})
	driver, err := coordinator.finalizationQueueDriver(call)
	require.NoError(t, err)
	require.Same(t, child, driver)
	require.NotSame(t, root, driver)
	legacy, err := coordinator.finalizationQueueDriver(SessionAgentCall{SessionID: "owner"})
	require.NoError(t, err)
	require.Same(t, root, legacy, "unmarked legacy dispatch is unchanged")
}

func TestFinalizationEnqueueCarriesOnlyCapturedProducerAuthority(t *testing.T) {
	fixture := newFinalizationDriverFixture(t)
	call := SessionAgentCall{
		SessionID: fixture.child.SessionID, LogicalCallID: fixture.child.LogicalCallID,
		Prompt: "compose settled results", finalizationAuthorities: fixture.capability,
	}
	raw, err := json.Marshal(ToSessionAgentCallData(call))
	require.NoError(t, err)
	sessions := session.NewService(db.New(fixture.conn), fixture.conn)
	require.NoError(t, enqueueFinalizationCall(context.Background(), sessions, "trusted-row", call, raw))
	err = sessions.EnqueueRunQueueEntry(context.Background(), "raw-row", call.SessionID, raw)
	require.ErrorIs(t, err, session.ErrAgentFinalizationChanged)
	var receipts int64
	require.NoError(t, fixture.conn.QueryRowContext(fixture.ctx, `SELECT count(*) FROM session_agent_finalization_continuations`).Scan(&receipts))
	require.EqualValues(t, 1, receipts)
}
