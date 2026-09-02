package agent

// R5-7 (P2 security review) regression test: RebuildSessionAgentCall used to
// construct its SessionAgentCall{...} return literal without ever assigning
// Origin, even though session.SessionAgentCallData.Origin explicitly promises
// to preserve the entry channel across processes and both generic DTO
// conversion functions (ToSessionAgentCallData/FromSessionAgentCallData)
// already copy it correctly. A row rebuilt by the durable pump therefore
// silently reverted to message.OriginUnspecified regardless of what actually
// entered the queue, making audit/transport metadata disagree with the
// request that was replayed.
//
// This test drives every non-zero message.Origin value used by the SDK/CLI/
// server entry points (message.OriginCLI, message.OriginWeb,
// message.OriginSDK — see internal/message/content.go) through the full
// durable-queue boundary: SessionAgentCall -> ToSessionAgentCallData -> JSON
// marshal/unmarshal -> RebuildSessionAgentCall, and asserts the rebuilt
// call's Origin matches the persisted row's for each one. It also covers the
// zero value to confirm a legacy/unspecified-origin row keeps rebuilding to
// OriginUnspecified rather than something else.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

func TestRebuildSessionAgentCall_RestoresOrigin(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	origins := []message.Origin{
		message.OriginUnspecified,
		message.OriginCLI,
		message.OriginWeb,
		message.OriginSDK,
	}

	for _, origin := range origins {
		origin := origin
		t.Run(string(origin), func(t *testing.T) {
			originalCall := SessionAgentCall{
				SessionID: "r5-7-origin-probe-" + string(origin),
				Prompt:    "hello",
				Origin:    origin,
			}

			// Real JSON round trip through the durable-queue boundary, not
			// just an in-memory struct copy — proves the DTO conversion AND
			// the rebuild path together, matching how a pump-driven restart
			// actually reads the row back from the DB.
			callData := ToSessionAgentCallData(originalCall)
			raw, err := json.Marshal(callData)
			require.NoError(t, err)
			var decoded session.SessionAgentCallData
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, origin, decoded.Origin,
				"the DTO conversion must preserve Origin across the JSON boundary")

			rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), decoded)
			require.NoError(t, err)
			assert.Equal(t, origin, rebuilt.Origin,
				"RebuildSessionAgentCall must restore the persisted row's Origin onto the rebuilt call")
		})
	}
}
