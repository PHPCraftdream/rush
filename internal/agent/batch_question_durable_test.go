// Task #37 regression tests: a batch delegation incarnation WITHOUT a
// held-question answer route must never hold or publish a question, must
// never advertise a usable raw ask tool, and must never mint answer
// authority for descendants — across replacement and durable queueing.
//
// The file pins five seams end to end:
//  1. Durable producers (restartOrphanedWithRetry, startDetachedRun) refuse
//     a SessionAgentCall carrying the process-local batch question
//     capability with ErrBatchQuestionCapabilityNotDurable, and enqueue NO
//     durable row (the capability is a live closure; a rebuilt call would
//     run as a non-batch session).
//  2. buildCall inherits the registered driver template's frozen capability
//     pointer and its shaped child toolset (deny: ask stripped; route-ful:
//     ask wrapped), while a session with NO registered driver stays
//     byte-identical (nil state, nil Tools).
//  3. InjectAgent's interrupt replacement carries the SAME pointer and
//     shaped toolset onto the replacement call the driver's mailbox
//     receives.
//  4. drainCallFor's wake path (callFor → newDrainCall) preserves the same
//     pointer and shaped toolset.
//  5. The REAL durable-payload writer (batchOneItemSpec, question_route
//     omitted) chained into the REAL executor guard: the ask fails fast
//     with ErrBatchQuestionNoRoute, no child_question notice is published,
//     the incarnation never holds, a follow-up inject is rejected, and the
//     scope settles with a failure report.
//
// All fixtures are lifted from the neighboring test files (p0_2/p0_3,
// drain_attempt_fixture_test.go, coordinator_agent_control_test.go,
// batch_exec_agent_test.go); no new test infra is introduced here.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// bqdAskViewBase builds the small ask+view toolset the shaping assertions
// run against, mirroring TestBuildBatchQuestionToolsRouteTable.
func bqdAskViewBase(t *testing.T) []fantasy.AgentTool {
	t.Helper()
	ask := fantasy.NewAgentTool(tools.AskQuestionToolName, "ask", func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	view := fantasy.NewAgentTool("view", "view", func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	return []fantasy.AgentTool{ask, view}
}

// bqdToolNames extracts tool names in order.
func bqdToolNames(list []fantasy.AgentTool) []string {
	out := make([]string, 0, len(list))
	for _, tool := range list {
		out = append(out, tool.Info().Name)
	}
	return out
}

// bqdShapedTools runs the production shaper for a deny (no-route) or a
// route-ful delegation context, mirroring runSubAgent's lifecycleBound
// shaping of call.Tools.
func bqdShapedTools(t *testing.T, route bool) []fantasy.AgentTool {
	t.Helper()
	ctx := withBatchQuestionRoute(context.Background(), route)
	if route {
		ctx = withBatchAgentQuestionBinding(ctx, batchAgentQuestionBinding{
			Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil },
		})
	}
	shaped, err := buildBatchQuestionTools(bqdAskViewBase(t), ctx)
	require.NoError(t, err)
	return shaped
}

// TestDurableProducersRefuseBatchQuestionCapability: the durable queue is a
// hard boundary for the batch question capability. Catches regressions
// where (a) the predicate callCarriesBatchQuestionCapability stopped
// matching (the producers would serialize a live closure into a rebuilt,
// capability-less call), or (b) restartOrphanedWithRetry /
// startDetachedRun stopped refusing capability-bearing calls and silently
// enqueued a durable row that would restart as a non-batch session (raw
// ask reachable, descendants minting route-ful leaves).
func TestDurableProducersRefuseBatchQuestionCapability(t *testing.T) {
	t.Run("unit_predicates", func(t *testing.T) {
		capabilityCall := SessionAgentCall{
			SessionID: "sess-cap",
			batchQuestion: &batchChildQuestionState{
				Depth: 2, Route: false, BindingOK: true,
				Binding: batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil }},
			},
		}
		require.True(t, callCarriesBatchQuestionCapability(capabilityCall),
			"a call with the frozen capability state must be reported as carrying it")
		require.False(t, callCarriesBatchQuestionCapability(SessionAgentCall{SessionID: "sess-plain"}),
			"a plain call must not be reported as carrying the capability")
		require.True(t, callCarriesNonDurableDependency(capabilityCall),
			"a capability-only call (no credentials, no disk provider) is still non-durable")
	})

	t.Run("restartOrphanedWithRetry_refuses", func(t *testing.T) {
		env := testEnv(t)
		sa := NewSessionAgent(SessionAgentOptions{
			SmartModel:   newFastSSEModel(t, "capability refusal"),
			FastModel:    newFastSSEModel(t, "capability refusal"),
			SystemPrompt: "test system prompt",
			Sessions:     env.sessions,
			Messages:     env.messages,
		}).(*sessionAgent)

		sess, err := env.sessions.Create(t.Context(), "bqd-orphan-capability-refusal")
		require.NoError(t, err)

		call := SessionAgentCall{
			SessionID:     sess.ID,
			Prompt:        "orphaned batch delegation restart",
			LogicalCallID: "bqd-capability-logical-id",
			batchQuestion: &batchChildQuestionState{Depth: 1, Route: false},
		}
		err = sa.restartOrphanedWithRetry([]SessionAgentCall{call})
		require.ErrorIs(t, err, ErrBatchQuestionCapabilityNotDurable,
			"restartOrphanedWithRetry must refuse a capability-bearing call outright")

		pending, listErr := env.sessions.ListPendingRunQueueEntries(context.Background())
		require.NoError(t, listErr)
		require.Empty(t, pending,
			"the refused call must NOT be durably enqueued: the row would rebuild as a non-batch session")
	})

	t.Run("startDetachedRun_refuses", func(t *testing.T) {
		env := testEnv(t)
		sa := NewSessionAgent(SessionAgentOptions{
			Sessions: env.sessions,
			Messages: env.messages,
		}).(*sessionAgent)
		coord := &coordinator{
			cfg:          &config.ConfigStore{},
			sessions:     env.sessions,
			messages:     env.messages,
			currentAgent: sa,
		}

		sess, err := env.sessions.Create(t.Context(), "bqd-detached-capability-refusal")
		require.NoError(t, err)

		call := SessionAgentCall{
			SessionID:     sess.ID,
			Prompt:        "detached batch delegation replacement",
			LogicalCallID: "bqd-detached-logical-id",
			batchQuestion: &batchChildQuestionState{Depth: 1, Route: true, BindingOK: true},
		}
		err = coord.startDetachedRun(context.Background(), call)
		require.ErrorIs(t, err, ErrBatchQuestionCapabilityNotDurable,
			"startDetachedRun must refuse a capability-bearing call before touching any state")

		pending, listErr := env.sessions.ListPendingRunQueueEntries(context.Background())
		require.NoError(t, listErr)
		require.Empty(t, pending,
			"the refused detached call must NOT be durably enqueued")
	})
}

// TestBuildCallInheritsBatchChildQuestionCapability: buildCall resolves the
// registered driver template's frozen question capability and shaped
// toolset onto a freshly built call (task #37 replacement path), while a
// session with NO registered driver stays byte-identical. Catches a
// regression where inheritBatchQuestionState stopped copying the SAME state
// pointer (a rebuilt call would lose the capability and run as a non-batch
// session) or stopped carrying the shaped Tools (ask reachable again).
func TestBuildCallInheritsBatchChildQuestionCapability(t *testing.T) {
	coord, env := newControlCoordinator(t)
	ctx := t.Context()

	parentID, denyChild := controlChild(t, env, "bqdeny")
	denyState := &batchChildQuestionState{Depth: 2, Route: false}
	coord.subAgentDrivers.register(denyChild, subAgentDriver{
		agent:           &mockSessionAgent{},
		call:            SessionAgentCall{SessionID: denyChild, batchQuestion: denyState, Tools: bqdShapedTools(t, false)},
		parentSessionID: parentID,
	})

	_, routeChild := controlChild(t, env, "bqroute")
	routeState := &batchChildQuestionState{
		Depth:     2,
		Route:     true,
		BindingOK: true,
		Binding:   batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil }},
	}
	coord.subAgentDrivers.register(routeChild, subAgentDriver{
		agent:           &mockSessionAgent{},
		call:            SessionAgentCall{SessionID: routeChild, batchQuestion: routeState, Tools: bqdShapedTools(t, true)},
		parentSessionID: parentID,
	})

	t.Run("deny_template_inherits_stripped_tools", func(t *testing.T) {
		pinned, err := coord.resolveSessionModels(ctx, denyChild)
		require.NoError(t, err)
		pinned.tools = bqdShapedTools(t, true)
		require.Contains(t, bqdToolNames(pinned.tools), tools.AskQuestionToolName)
		built, err := coord.buildCall(ctx, denyChild, "p", pinned, nil)
		require.NoError(t, err)
		require.Same(t, denyState, built.batchQuestion,
			"the built call must carry the SAME frozen capability pointer as the template")
		require.NotContains(t, bqdToolNames(built.Tools), tools.AskQuestionToolName,
			"a no-route delegation's replacement call must advertise NO ask_question")
		require.Contains(t, bqdToolNames(built.Tools), "view", "the other tool must survive")
	})

	t.Run("routeful_template_inherits_wrapped_ask", func(t *testing.T) {
		pinned, err := coord.resolveSessionModels(ctx, routeChild)
		require.NoError(t, err)
		pinned.tools = bqdShapedTools(t, false)
		require.NotContains(t, bqdToolNames(pinned.tools), tools.AskQuestionToolName)
		built, err := coord.buildCall(ctx, routeChild, "p", pinned, nil)
		require.NoError(t, err)
		require.Same(t, routeState, built.batchQuestion,
			"the built call must carry the SAME frozen capability pointer as the template")
		var wrapped bool
		for _, tool := range built.Tools {
			if tool.Info().Name != tools.AskQuestionToolName {
				continue
			}
			_, wrapped = tool.(batchAgentAskQuestionTool)
			require.True(t, wrapped,
				"a route-ful delegation's replacement ask_question must be the held-answer bridge wrapper")
		}
		require.True(t, wrapped, "ask_question must still be advertised for the route-ful delegation")
	})

	t.Run("no_registered_driver_byte_identical", func(t *testing.T) {
		pinned, err := coord.resolveSessionModels(ctx, parentID)
		require.NoError(t, err)
		built, err := coord.buildCall(ctx, parentID, "p", pinned, nil)
		require.NoError(t, err)
		require.Nil(t, built.batchQuestion,
			"a session with no registered driver must build a legacy capability-less call")
		require.Equal(t, pinned.tools, built.Tools,
			"a session with no registered driver must preserve its resolved toolset")
	})
}

// TestInjectAgentReplacementInheritsChildCapability: InjectAgent's interrupt
// replacement (non-credentialed branch → buildCall → InterruptAndReplace)
// must hand the driver's mailbox a replacement call carrying the SAME
// frozen capability pointer and shaped toolset. Catches a regression where
// the replacement lost the capability (a replaced delegation turn regains
// the raw ask tool and mints route-ful descendant leaves) or the shaped
// toolset.
func TestInjectAgentReplacementInheritsChildCapability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route bool
	}{
		{name: "deny_replacement_ask_stripped", route: false},
		{name: "routeful_replacement_ask_wrapped", route: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coord, env := newControlCoordinator(t)
			parentID, childID := controlChild(t, env, tc.name)
			ctx := t.Context()

			state := &batchChildQuestionState{Depth: 1, Route: tc.route, BindingOK: tc.route}
			if tc.route {
				state.Binding = batchAgentQuestionBinding{
					Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil },
				}
			}
			spy := &mockSessionAgent{}
			coord.subAgentDrivers.register(childID, subAgentDriver{
				agent:           spy,
				call:            SessionAgentCall{SessionID: childID, batchQuestion: state, Tools: bqdShapedTools(t, tc.route)},
				parentSessionID: parentID,
			})

			// The child driver's credentials are nil, so InjectAgent takes
			// the non-credentialed branch: buildCall + InterruptAndReplace.
			// The spy's InterruptAndReplace returns true synchronously, so
			// the capture is complete when InjectAgent returns.
			_, err := coord.InjectAgent(ctx, parentID, childID, "replacement prompt", true)
			require.NoError(t, err)

			captured := spy.interruptAndReplacedSnapshot()
			require.Len(t, captured, 1,
				"the interrupt replacement must reach the child's driver agent exactly once")
			replacement := captured[0]
			require.Same(t, state, replacement.batchQuestion,
				"the replacement call must carry the SAME frozen capability pointer")
			require.Equal(t, "replacement prompt", replacement.Prompt)
			if tc.route {
				var wrapped bool
				for _, tool := range replacement.Tools {
					if tool.Info().Name != tools.AskQuestionToolName {
						continue
					}
					_, wrapped = tool.(batchAgentAskQuestionTool)
					require.True(t, wrapped, "the route-ful replacement's ask must stay wrapped")
				}
				require.True(t, wrapped, "the route-ful replacement must still advertise ask_question")
			} else {
				require.NotContains(t, bqdToolNames(replacement.Tools), tools.AskQuestionToolName,
					"the deny replacement must advertise NO ask_question")
			}
		})
	}
}

// TestDrainCallForCarriesDriverCapability: drainCallFor's driver branch
// (callFor → newDrainCall) must preserve the registered template's frozen
// capability pointer and shaped toolset onto every automatic wake turn.
// Catches a regression where a wake rebuilt the child as a non-batch
// session (the wake arrives on a background callback context that carries
// none of the capability, so the template is the only carrier).
func TestDrainCallForCarriesDriverCapability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route bool
	}{
		{name: "deny_wake_ask_stripped", route: false},
		{name: "routeful_wake_ask_wrapped", route: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttemptFixture(t, "bqd-drain-"+tc.name, attemptFixtureOpts{noIdle: true})
			ctx := context.Background()

			state := &batchChildQuestionState{Depth: 1, Route: tc.route, BindingOK: tc.route}
			if tc.route {
				state.Binding = batchAgentQuestionBinding{
					Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil },
				}
			}
			// Re-register the fixture's own driver slot with a capability
			// template: the same shape runSubAgent registers at delegation
			// start.
			f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{
				agent: f.sa,
				call:  SessionAgentCall{SessionID: f.sessID, SmartModel: &f.model, batchQuestion: state, Tools: bqdShapedTools(t, tc.route)},
			})

			call, prep, err := f.coord.drainCallFor(ctx, f.sessID)
			require.NoError(t, err)
			require.True(t, call.IsDrain)
			require.Same(t, state, call.batchQuestion,
				"the wake call must preserve the SAME frozen capability pointer as the template")
			if tc.route {
				var wrapped bool
				for _, tool := range call.Tools {
					if tool.Info().Name != tools.AskQuestionToolName {
						continue
					}
					_, wrapped = tool.(batchAgentAskQuestionTool)
					require.True(t, wrapped, "the route-ful wake's ask must stay wrapped")
				}
				require.True(t, wrapped, "the route-ful wake must still advertise ask_question")
			} else {
				require.NotContains(t, bqdToolNames(call.Tools), tools.AskQuestionToolName,
					"the deny wake must advertise NO ask_question")
			}
			if prep != nil {
				prep.release()
			}
		})
	}
}

// TestBatchOneItemSpecPayloadRunsNoRouteInRealExecutor: the REAL
// durable-payload writer (batchOneItemSpec, question_route omitted per the
// omitempty contract) chained into the REAL executor runtime guard. This
// proves writer → durable JSON → executor guard end to end, not hand-set
// booleans. Mirrors TestBatchAskWithoutRouteRejectsBeforeHoldAndNotice,
// which catches a regression where askBatchQuestion held the scope or
// published a notice BEFORE checking the durable payload's route.
func TestBatchOneItemSpecPayloadRunsNoRouteInRealExecutor(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	askErr := make(chan error, 1)
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		// The ask must not run on the Start goroutine: it would park Start.
		go func() {
			_, err := host.Ask(context.Background(), 2, "why?")
			askErr <- err
			// Mirror askAndShape's error branch: the turn finishes with an
			// error response and exits, so the node settles.
			t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextErrorResponse(err.Error())}, nil)
			t2.finish()
			t2.exitNow()
		}()
	}

	// The durable payload comes from the production direct-call writer with
	// QuestionRoute:false — the key is omitted from the marshaled JSON.
	inv := BatchInvocation{
		SessionID:     "owner-bqd-exec",
		Origin:        "coordinator",
		QuestionRoute: false,
	}
	spec, err := batchOneItemSpec(inv, fantasy.ToolCall{ID: "call-nr-exec", Name: "agent"}, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.NotNil(t, spec.Task)
	var payloadFields map[string]any
	require.NoError(t, json.Unmarshal(spec.Task.Payload, &payloadFields))
	_, routePresent := payloadFields["question_route"]
	require.False(t, routePresent,
		"the writer's no-route payload must omit question_route entirely (omitempty)")

	e, factory, reader, reports := batchAgentTestRunWithPayload(t, batchAgentDeps{}, factory, spec.Task.Payload)

	// (a) The ask itself failed with the sentinel, deterministically.
	require.Eventually(t, func() bool { return len(askErr) > 0 }, 2*time.Second, time.Millisecond,
		"the ask must return instead of awaiting forever")
	err = <-askErr
	tl.addf("ask rejected: err=%v", err)
	require.ErrorIs(t, err, ErrBatchQuestionNoRoute)

	// (b) NO child_question notice was published.
	notices := factory.questionRig().publishedNotices()
	tl.addf("published notices: %d", len(notices))
	require.Empty(t, notices, "a no-route ask must never publish a child_question notice")

	// (c) The incarnation never held: after the turn settled, heldDepth is (0,false).
	_, held := e.heldDepth()
	tl.addf("heldDepth after settle: held=%v", held)
	require.False(t, held, "a no-route ask must never hold the scope")

	// (d) The scope report DOES arrive and is failure-shaped: the node
	// settles, no eternal await, no fake success.
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	tl.addf("scope report: state=%s code=%s", r.State, r.Code)
	require.Equal(t, jobbatch.StateFailed, r.State)
	require.Equal(t, "agent_error", r.Code)

	// (e) Deterministic negative: the incarnation never held, so a
	// follow-up inject is rejected (NotHeld while live, stale after
	// terminality) — it must never be deliverable.
	injectErr := jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":2,"sequence":1,"text":"late"}`))
	tl.addf("follow-up inject: err=%v", injectErr)
	require.Error(t, injectErr, "a no-route incarnation never held, so nothing is deliverable")
	require.True(t, errors.Is(injectErr, ErrBatchAgentNotHeld) || errors.Is(injectErr, ErrBatchAgentStaleGeneration),
		"the inject must be rejected as NotHeld or stale, got %v", injectErr)
	require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
}
