package agent

// The #37 answer-route contract, pinned end to end: a standalone unattended
// batch incarnation whose durable payload carries NO question_route (or
// false) never holds a question, never publishes a child_question notice,
// never awaits forever and never fakes success — the ask fails fast with
// ErrBatchQuestionNoRoute and the node settles as a failure. A route-ful
// (parent) incarnation keeps its hold/publish/answer path byte-identical.
// The route is stamped only by trusted intake (the durable payload), never
// by model input: forged question_route keys inside leaf tool input are
// ignored. The ask wrapper maps the no-route sentinel to a RETRYABLE tool
// error (no Go error, no StopTurn); the toolset builder strips ask_question
// for no-route incarnations and wraps it only for route-ful ones; and the
// AllowedTools floor strips ask_question last so an explicit model role can
// never restore it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchAskWithoutRouteRejectsBeforeHoldAndNotice: a payload WITHOUT
// question_route (rebuilt stale payload, standalone unattended batch) asks
// through the host. Catches a regression where askBatchQuestion held the
// scope or published a notice BEFORE checking the route: the ask must fail
// fast with ErrBatchQuestionNoRoute, no child_question notice may exist, the
// incarnation must never report held, the scope must settle with a failure
// report (no eternal await), and a follow-up inject must be rejected — the
// incarnation never held, so there is nothing to answer into.
func TestBatchAskWithoutRouteRejectsBeforeHoldAndNotice(t *testing.T) {
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
	// The shared rig, but with a payload whose question_route is ABSENT.
	payload, err := json.Marshal(batchLeafPayload{Tool: "agent", ToolCallID: "call", SessionID: t.Name(), Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	e, factory, reader, reports := batchAgentTestRunWithPayload(t, batchAgentDeps{}, factory, payload)

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

// TestBatchAskWrapperNoRouteIsRetryable: the ask_question wrapper receives a
// WRAPPED ErrBatchQuestionNoRoute from the binding. Catches a regression
// where the mapping in batchAgentAskQuestionTool.Run was dropped or made the
// sentinel a fatal (Go error / StopTurn) result: the sentinel must map to a
// retryable text-error response (no Go error, no StopTurn) so the model can
// finish its work without asking, and the binding must actually have been
// reached (this pins the mapping, not the advertisement).
func TestBatchAskWrapperNoRouteIsRetryable(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	asked := make(chan struct{}, 1)
	binding := batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
		asked <- struct{}{}
		return nil, fmt.Errorf("%w", ErrBatchQuestionNoRoute)
	}}
	ctx := withBatchAgentQuestionContext(withBatchAgentQuestionBinding(t.Context(), binding), 1)
	tool := batchAgentAskQuestionTool{}
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: "ask_question", Input: `{"question":"continue?"}`})
	tl.addf("no-route ask: err=%v is_error=%v stop_turn=%v", err, response.IsError, response.StopTurn)
	require.NoError(t, err, "the no-route sentinel must map to a retryable tool response, not a Go error")
	require.True(t, response.IsError)
	require.False(t, response.StopTurn, "a no-route refusal must not end the turn")
	select {
	case <-asked:
	default:
		t.Fatal("the binding ask was never reached: the mapping test must exercise the runtime path")
	}
}

// TestBuildBatchQuestionToolsRouteTable: the per-call child toolset shaping
// across the three route contexts. Catches regressions where (a) a no-route
// incarnation still advertises ask_question, (b) a route-ful incarnation
// gets the RAW ask tool instead of the held-answer bridge wrapper, or (c)
// the builder mutates/drops tools for a plain non-batch context.
func TestBuildBatchQuestionToolsRouteTable(t *testing.T) {
	ask := fantasy.NewAgentTool(tools.AskQuestionToolName, "ask", func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	view := fantasy.NewAgentTool("view", "view", func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	base := []fantasy.AgentTool{ask, view}
	names := func(list []fantasy.AgentTool) []string {
		out := make([]string, 0, len(list))
		for _, tool := range list {
			out = append(out, tool.Info().Name)
		}
		return out
	}

	t.Run("no_route_strips_ask", func(t *testing.T) {
		got, err := buildBatchQuestionTools(base, withBatchQuestionRoute(context.Background(), false))
		require.NoError(t, err)
		require.NotContains(t, names(got), tools.AskQuestionToolName,
			"a no-route incarnation must advertise NO ask_question")
		require.Contains(t, names(got), "view", "the other tool must survive the strip")
	})
	t.Run("route_wraps_ask", func(t *testing.T) {
		ctx := withBatchQuestionRoute(context.Background(), true)
		ctx = withBatchAgentQuestionBinding(ctx, batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil }})
		got, err := buildBatchQuestionTools(base, ctx)
		require.NoError(t, err)
		require.Len(t, got, 2)
		var wrapped bool
		for _, tool := range got {
			if tool.Info().Name != tools.AskQuestionToolName {
				continue
			}
			_, wrapped = tool.(batchAgentAskQuestionTool)
			require.True(t, wrapped,
				"a route-ful incarnation's ask_question must be the held-answer bridge wrapper")
		}
		require.True(t, wrapped, "ask_question must still be present")
	})
	t.Run("no_marker_unchanged", func(t *testing.T) {
		got, err := buildBatchQuestionTools(base, context.Background())
		require.NoError(t, err)
		require.Len(t, got, 2, "a non-batch context must leave the list unchanged")
		require.Equal(t, []string{tools.AskQuestionToolName, "view"}, names(got))
		for _, tool := range got {
			_, isWrapper := tool.(batchAgentAskQuestionTool)
			require.False(t, isWrapper, "no binding present: nothing may be wrapped")
		}
	})
}

// TestStartBatchAgentTurnRouteMarkerAndBinding: the production driver's
// stamping. Catches a regression where startBatchAgentTurn stopped stamping
// withBatchQuestionRoute (the toolset builder would then treat a no-route
// batch child as a non-batch session and leave ask_question reachable) or
// armed the held-answer binding for a no-route incarnation (a working-looking
// ask on an unattended batch child). The leaf-captured context is
// mutex-protected (the leaf runs on the driver's goroutine) and the turn is
// joined via its Done channel, so the race-detector-clean shape holds.
func TestStartBatchAgentTurnRouteMarkerAndBinding(t *testing.T) {
	for _, route := range []bool{false, true} {
		t.Run(fmt.Sprintf("question_route_%v", route), func(t *testing.T) {
			tl := newQuestionTestTimeline(t)
			var mu sync.Mutex
			var captured context.Context
			leaf := fantasy.NewAgentTool("agent", "leaf", func(ctx context.Context, _ map[string]any, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				mu.Lock()
				captured = ctx
				mu.Unlock()
				return fantasy.NewTextResponse("done"), nil
			})
			host := batchAgentHost{Ask: func(context.Context, int, string) (json.RawMessage, error) {
				return nil, errors.New("host ask must not run in this test")
			}}
			req := batchAgentRunRequest{Tool: "agent", Input: json.RawMessage(`{}`), QuestionRoute: route}
			// The leaf runs on the stamped run context in the driver's
			// goroutine; the driver settles itself once the leaf returns.
			tl.addf("driver request: route=%v tool=%s input=%s", route, req.Tool, req.Input)
			turn, err := startBatchAgentTurn(context.Background(), req, host, leaf, nil, func() {})
			require.NoError(t, err)
			t.Cleanup(func() {
				if t.Failed() {
					tl.addf("driver result: err=%v result=%+v", turn.Err(), turn.Result())
				}
			})
			capturedCtx := func() context.Context {
				mu.Lock()
				defer mu.Unlock()
				return captured
			}
			require.Eventually(t, func() bool { return capturedCtx() != nil }, 2*time.Second, time.Millisecond,
				"the leaf must run on the stamped run context")
			stampedRoute, routeOK := batchQuestionRouteFrom(capturedCtx())
			_, bindingOK := batchAgentQuestionBindingFrom(capturedCtx())
			tl.addf("stamped: route_ok=%v route=%v binding=%v", routeOK, stampedRoute, bindingOK)
			require.True(t, routeOK, "the marker must be stamped on EVERY batch incarnation, both values")
			require.Equal(t, route, stampedRoute, "the marker must carry the request's route verbatim")
			require.Equal(t, route, bindingOK,
				"the held-answer binding must be armed ONLY for route-ful incarnations")
			// Join the turn: the driver's goroutine must settle cleanly.
			select {
			case <-turn.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the batch turn never finished: the driver goroutine must settle itself")
			}
			require.NoError(t, turn.Err(), "a successful leaf must leave the turn error-free")
		})
	}
}

// TestModelBatchSpecQuestionRouteStampingAndForgedInput: the invocation's
// trusted route stamps every agent leaf payload. Catches regressions where
// (a) a no-route caller minted route-ful leaves (a nested batch would regain
// an answer route it never had — direct-path intake losing the route), (b)
// the omitempty contract broke and a route-less payload carried an explicit
// false (a rebuilt payload would look route-ful to older readers), or (c)
// forged question_route keys inside the model's leaf tool input leaked into
// the payload — the route follows the invocation, never the input. Every
// subtest routes through modelInvocationAtCall FIRST: the route can only
// ever come from the call context.
func TestModelBatchSpecQuestionRouteStampingAndForgedInput(t *testing.T) {
	payloadOf := func(t *testing.T, inv BatchInvocation, item batchModelItem) map[string]any {
		t.Helper()
		spec, err := modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{item}}, inv, modelRegistry("view"))
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(spec.Batch.Items[0].Task.Payload, &payload))
		return payload
	}
	t.Run("top_level_default_true", func(t *testing.T) {
		// A plain top-level session context (no marker): the session's own
		// operator surface IS the established answer route.
		inv, err := modelInvocationAtCall(batchCallCtx("owner"), BatchInvocation{SessionID: "owner", RegistryID: "reg"}, fantasy.ToolCall{ID: "call-dt", Name: "batch_run"})
		require.NoError(t, err)
		require.True(t, inv.QuestionRoute,
			"an absent marker must default the invocation to route-ful (top-level sessions answer)")
		payload := payloadOf(t, inv, batchModelItem{Tool: "view", Input: json.RawMessage(`{"path":"x"}`)})
		require.Equal(t, true, payload["question_route"],
			"a route-ful leaf payload must carry question_route=true")
	})
	t.Run("batch_child_inherits_false", func(t *testing.T) {
		// A no-route batch incarnation's control call: the child must
		// inherit the false, so its leaf payload OMITS the key entirely.
		inv, err := modelInvocationAtCall(withBatchQuestionRoute(batchCallCtx("owner"), false), BatchInvocation{SessionID: "owner", RegistryID: "reg"}, fantasy.ToolCall{ID: "call-nr", Name: "batch_run"})
		require.NoError(t, err)
		require.False(t, inv.QuestionRoute,
			"the present-false marker must win over the default: direct-path intake must not lose the route")
		payload := payloadOf(t, inv, batchModelItem{Tool: "view", Input: json.RawMessage(`{"path":"x"}`)})
		_, present := payload["question_route"]
		require.False(t, present,
			"a no-route leaf payload must OMIT the key entirely (omitempty), not carry an explicit false")
	})
	t.Run("forged_input_ignored", func(t *testing.T) {
		// The model forges question_route inside the leaf tool's input AND
		// the route the invocation actually carries is false: the forged
		// key survives only as verbatim leaf input, never as payload policy.
		inv, err := modelInvocationAtCall(withBatchQuestionRoute(batchCallCtx("owner"), false), BatchInvocation{SessionID: "owner", RegistryID: "reg"}, fantasy.ToolCall{ID: "call-forged", Name: "batch_run"})
		require.NoError(t, err)
		require.False(t, inv.QuestionRoute)
		payload := payloadOf(t, inv, batchModelItem{Tool: "view", Input: json.RawMessage(`{"path":"x","question_route":true}`)})
		_, present := payload["question_route"]
		require.False(t, present,
			"the payload route must follow the trusted invocation (false/omitted), never the forged input key")
		// The canonical input keeps the model's forged key VERBATIM (audit
		// fidelity), while the payload policy stays route-less.
		rawInput, err := json.Marshal(payload["input"])
		require.NoError(t, err)
		require.Contains(t, string(rawInput), `"question_route":true`,
			"the canonical leaf input must keep the model's forged key verbatim for audit")
	})
}

// TestApplyBatchQuestionRouteFloor: the final AllowedTools floor. Catches
// regressions where the floor stopped stripping ask_question for no-route
// incarnations (an explicit model role or worker layering could restore the
// stale tool) or stripped tools for non-batch/route-ful builds (every
// non-batch and route-ful build must stay byte-identical, same names, same
// order).
func TestApplyBatchQuestionRouteFloor(t *testing.T) {
	base := []string{"view", tools.AskQuestionToolName, "todos", tools.AskQuestionToolName, "fetch"}
	t.Run("marker_absent_unchanged", func(t *testing.T) {
		got := applyBatchQuestionRouteFloor(context.Background(), config.Agent{AllowedTools: base})
		require.Equal(t, base, got.AllowedTools,
			"a non-batch session's AllowedTools must stay byte-identical (names and order)")
	})
	t.Run("marker_false_strips_even_worker_layered", func(t *testing.T) {
		// base already contains ask_question twice: the worker layering
		// shape. An explicit model role can never restore the tool.
		got := applyBatchQuestionRouteFloor(withBatchQuestionRoute(context.Background(), false), config.Agent{AllowedTools: base})
		require.Equal(t, []string{"view", "todos", "fetch"}, got.AllowedTools,
			"every ask_question occurrence must be stripped, others preserved in order")
	})
	t.Run("marker_true_unchanged", func(t *testing.T) {
		got := applyBatchQuestionRouteFloor(withBatchQuestionRoute(context.Background(), true), config.Agent{AllowedTools: base})
		require.Equal(t, base, got.AllowedTools,
			"a route-ful build must stay byte-identical (names and order)")
	})
}

// TestInvokeBatchWithCallContextStampsQuestionRoute: the direct-call
// ingress adapter's route stamping. Catches a regression where
// invokeBatchWithCallContext stopped reading the context route marker and
// stopped defaulting true — direct-path intake losing the route: a
// parent-owned agent delegation would silently lose its answer route
// (plain ctx) or a no-route child would mint route-ful leaves (marker
// false). The recorded effects come from the ingress stub, never from the
// adapter's return path alone.
func TestInvokeBatchWithCallContextStampsQuestionRoute(t *testing.T) {
	leaf := fantasy.NewAgentTool("agent", "leaf", func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	run := func(t *testing.T, ctx context.Context) BatchInvocation {
		t.Helper()
		ingress := &routeCaptureIngress{}
		tpl := BatchInvocation{SessionID: "owner", RegistryID: "reg-1", Origin: "coordinator", Policy: batchLeafPolicyContext{RegistryID: "reg-1", RestrictedToolset: "reg-1"}}
		_, err := invokeBatchWithCallContext(ctx, ingress, tpl, leaf, fantasy.ToolCall{ID: "call", Name: "agent", Input: `{"prompt":"x"}`})
		require.NoError(t, err)
		inv, count := ingress.recorded()
		require.Equal(t, 1, count, "the adapter must hand the invocation to the ONE authority exactly once")
		return inv
	}
	t.Run("plain_ctx_defaults_true", func(t *testing.T) {
		// A top-level session: the absent marker must default the route ON.
		inv := run(t, context.Background())
		require.True(t, inv.QuestionRoute,
			"direct-path intake must default a top-level session's invocation to route-ful")
	})
	t.Run("marker_false_inherits_false", func(t *testing.T) {
		// A no-route batch incarnation calling directly: the marker wins.
		inv := run(t, withBatchQuestionRoute(context.Background(), false))
		require.False(t, inv.QuestionRoute,
			"the present-false marker must reach the ingress verbatim, or a no-route child mints route-ful leaves")
	})
	t.Run("marker_true_inherits_true", func(t *testing.T) {
		inv := run(t, withBatchQuestionRoute(context.Background(), true))
		require.True(t, inv.QuestionRoute,
			"the present-true marker must reach the ingress verbatim")
	})
}

// routeCaptureIngress records the invocations handed over Invoke under a
// mutex (they arrive on the caller's goroutine here, but the shape keeps
// the cross-goroutine contract), so assertions read recorded effects.
type routeCaptureIngress struct {
	mu      sync.Mutex
	invokes int
	lastInv BatchInvocation
}

func (r *routeCaptureIngress) Invoke(_ context.Context, inv BatchInvocation, _ fantasy.AgentTool, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invokes++
	r.lastInv = inv
	return fantasy.NewTextResponse("ok"), nil
}

func (r *routeCaptureIngress) recorded() (BatchInvocation, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastInv, r.invokes
}

func (r *routeCaptureIngress) PinLeafRegistry(*BatchLeafRegistry) (string, error) {
	return "", errors.New("not supported")
}

func (r *routeCaptureIngress) ResolveLeaf(string, string) (fantasy.AgentTool, error) {
	return nil, errors.New("not supported")
}

func (r *routeCaptureIngress) Registry(string) (*BatchLeafRegistry, error) {
	return nil, errors.New("not supported")
}

// TestBatchOneItemSpecCarriesQuestionRoute: the durable payload writer.
// Catches a regression where batchOneItemSpec stopped copying
// inv.QuestionRoute into batchLeafPayload — direct-path intake losing the
// route at the durable boundary: a parent's direct agent delegation would
// rebuild as route-less (no answers ever) or a no-route child would
// rebuild route-ful. Asserted on the marshaled payload's key PRESENCE.
func TestBatchOneItemSpecCarriesQuestionRoute(t *testing.T) {
	call := fantasy.ToolCall{ID: "call", Name: "agent"}
	input := json.RawMessage(`{"prompt":"x"}`)
	decode := func(t *testing.T, inv BatchInvocation) map[string]any {
		t.Helper()
		spec, err := batchOneItemSpec(inv, call, input)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(spec.Task.Payload, &payload))
		return payload
	}
	t.Run("false_key_absent", func(t *testing.T) {
		// The omitempty contract: a no-route payload must OMIT the key
		// entirely so older readers and rebuilt payloads stay unambiguous.
		payload := decode(t, BatchInvocation{SessionID: "owner", RegistryID: "reg", QuestionRoute: false})
		_, present := payload["question_route"]
		require.False(t, present,
			"a no-route leaf payload must omit question_route entirely (omitempty)")
	})
	t.Run("true_key_present", func(t *testing.T) {
		payload := decode(t, BatchInvocation{SessionID: "owner", RegistryID: "reg", QuestionRoute: true})
		require.Equal(t, true, payload["question_route"],
			"a route-ful leaf payload must carry question_route=true so the child keeps its answer route")
	})
}

// TestBatchAskWrapperNoRouteWithoutBindingNeverReachesRawAsk: the stale
// wrapper's guard precedence against the raw force-finish tool. Catches a
// regression where batchAgentAskQuestionTool.Run checked the binding
// fallback BEFORE the route marker — a stale wrapper bypassing the guard
// via the binding fallback: (1) a no-route, binding-less incarnation must
// fail retryable WITHOUT delegating to the raw tool, (2) a non-batch
// (marker-less) incarnation must keep the raw legacy contract, (3) a
// route-ful incarnation whose binding reports the sentinel must map it
// retryable without ever falling back to the raw tool.
func TestBatchAskWrapperNoRouteWithoutBindingNeverReachesRawAsk(t *testing.T) {
	var mu sync.Mutex
	var reached int
	raw := fantasy.NewAgentTool(tools.AskQuestionToolName, "raw ask", func(_ context.Context, _ map[string]any, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		mu.Lock()
		reached++
		mu.Unlock()
		// The real raw tool force-finishes; the sentinel error is its
		// observable legacy contract.
		return fantasy.ToolResponse{}, &tools.AskQuestionError{Question: "why?", SessionID: "child"}
	})
	reachedCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reached
	}
	wrapper := batchAgentAskQuestionTool{AgentTool: raw}
	run := func(t *testing.T, ctx context.Context) (fantasy.ToolResponse, error) {
		t.Helper()
		return wrapper.Run(ctx, fantasy.ToolCall{Name: tools.AskQuestionToolName, Input: `{"question":"continue?"}`})
	}
	t.Run("marker_false_no_binding_retryable_raw_not_reached", func(t *testing.T) {
		// A no-route incarnation with NO binding: the guard must fire
		// before the binding fallback hands off to the raw tool.
		resp, err := run(t, withBatchQuestionRoute(context.Background(), false))
		require.NoError(t, err, "the guard must refuse as a retryable tool response, not a Go error")
		require.True(t, resp.IsError)
		require.False(t, resp.StopTurn, "a no-route refusal must not end the turn")
		require.Equal(t, 0, reachedCount(),
			"the raw ask tool must never run for a no-route incarnation: its force-finish would park the run forever")
	})
	t.Run("marker_absent_no_binding_legacy_raw_contract", func(t *testing.T) {
		// A non-batch session (no marker): the wrapper must transparently
		// delegate — the raw force-finish contract survives.
		_, err := run(t, context.Background())
		var askErr *tools.AskQuestionError
		require.True(t, errors.As(err, &askErr),
			"a non-batch session must keep the legacy raw contract (AskQuestionError), got %v", err)
		require.Equal(t, 1, reachedCount(),
			"the legacy delegation must actually reach the raw ask tool")
	})
	t.Run("marker_true_binding_no_route_retryable_raw_not_reached", func(t *testing.T) {
		// A route-ful wrapper whose binding reports the wrapped sentinel:
		// the mapping fires and the raw tool is never consulted.
		ctx := withBatchQuestionRoute(context.Background(), true)
		ctx = withBatchAgentQuestionContext(ctx, 1)
		ctx = withBatchAgentQuestionBinding(ctx, batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
			return nil, fmt.Errorf("%w", ErrBatchQuestionNoRoute)
		}})
		resp, err := run(t, ctx)
		require.NoError(t, err, "the wrapped sentinel must map to a retryable tool response")
		require.True(t, resp.IsError)
		require.False(t, resp.StopTurn, "the sentinel mapping must not end the turn")
		require.Equal(t, 1, reachedCount(),
			"the raw tool must not be reached on the binding path (only the legacy subtest ran it)")
	})
}

// TestArmBatchQuestionTurnContextReArmsCapability: runTurn's re-arming of
// the frozen delegation state onto every turn context. Catches a regression
// where armBatchQuestionTurnContext stopped stamping the route marker,
// depth or binding from call.batchQuestion — wake turns arriving on a
// capability-less background context: a wake Drain would then run as a
// non-batch session (raw ask reachable) or as a route-less child (answers
// lost). Asserted on the rebuilt context's marker/depth/binding values.
func TestArmBatchQuestionTurnContextReArmsCapability(t *testing.T) {
	binding := batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil }}
	t.Run("nil_state_untouched", func(t *testing.T) {
		// A non-batch call: nothing is stamped, legacy contracts stand.
		base := context.WithValue(context.Background(), struct{ k string }{}, "marker")
		ctx := armBatchQuestionTurnContext(base, SessionAgentCall{})
		_, routeOK := batchQuestionRouteFrom(ctx)
		require.False(t, routeOK, "a state-less call must stay a non-batch session (no route marker)")
		require.Equal(t, 0, batchAgentQuestionDepth(ctx),
			"a state-less call must carry no question depth")
		require.Equal(t, "marker", ctx.Value(struct{ k string }{}),
			"the untouched context must be returned, not a rebuilt one")
	})
	t.Run("route_ful_state_fully_armed", func(t *testing.T) {
		call := SessionAgentCall{batchQuestion: &batchChildQuestionState{Depth: 3, Route: true, Binding: binding, BindingOK: true}}
		ctx := armBatchQuestionTurnContext(context.Background(), call)
		route, ok := batchQuestionRouteFrom(ctx)
		require.True(t, ok, "the route marker must be stamped for a batch call")
		require.True(t, route, "the frozen route must re-arm verbatim")
		require.Equal(t, 3, batchAgentQuestionDepth(ctx), "the frozen depth must re-arm verbatim")
		stamped, bindingOK := batchAgentQuestionBindingFrom(ctx)
		require.True(t, bindingOK, "a binding-armed state must re-arm its binding")
		require.NotNil(t, stamped.Ask, "the re-armed binding must carry the frozen Ask func")
	})
	t.Run("no_route_no_binding_state", func(t *testing.T) {
		call := SessionAgentCall{batchQuestion: &batchChildQuestionState{Depth: 1, Route: false, BindingOK: false}}
		ctx := armBatchQuestionTurnContext(context.Background(), call)
		route, ok := batchQuestionRouteFrom(ctx)
		require.True(t, ok, "the marker must be stamped even when false: a batch child is not a non-batch session")
		require.False(t, route, "the frozen false route must re-arm verbatim")
		require.Equal(t, 1, batchAgentQuestionDepth(ctx), "the frozen depth must re-arm verbatim")
		_, bindingOK := batchAgentQuestionBindingFrom(ctx)
		require.False(t, bindingOK,
			"a BindingOK=false state must never arm a binding: a no-route child has no answer route")
	})
}

// TestBatchQuestionStateSurvivesWakeConstructors: the call-template
// constructors behind wake turns. Catches a regression where callFromActive,
// newDrainCall, applyDrainModelOptions or subAgentDriver.callFor rebuilt a
// SessionAgentCall WITHOUT carrying batchQuestion — wake turns arriving on
// a capability-less background context: the delegation's Drain would lose
// its route/binding/depth and fall back to the raw legacy ask. The SAME
// state pointer and its values must survive every constructor; a state-less
// call must stay state-less.
func TestBatchQuestionStateSurvivesWakeConstructors(t *testing.T) {
	binding := batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, nil }}
	routeState := &batchChildQuestionState{Depth: 3, Route: true, Binding: binding, BindingOK: true}
	noRouteState := &batchChildQuestionState{Depth: 1, Route: false}
	for name, st := range map[string]*batchChildQuestionState{"route_ful": routeState, "no_route": noRouteState} {
		t.Run(name, func(t *testing.T) {
			template := SessionAgentCall{SessionID: "child", batchQuestion: st}
			fromActive := callFromActive(template)
			require.Same(t, st, fromActive.batchQuestion,
				"callFromActive must preserve the SAME question state pointer")
			drain := newDrainCall(template)
			require.Same(t, st, drain.batchQuestion,
				"newDrainCall must preserve the SAME question state pointer")
			drainFromActive := newDrainCall(callFromActive(template))
			require.Same(t, st, drainFromActive.batchQuestion,
				"the composed wake constructor path must preserve the SAME question state pointer")
			// applyDrainModelOptions takes the call BY VALUE and returns the
			// adjusted copy: the state pointer must survive model re-pinning.
			modeled := applyDrainModelOptions(template, Model{ModelCfg: config.SelectedModel{Provider: "prov", Model: "m"}})
			require.Same(t, st, modeled.batchQuestion,
				"applyDrainModelOptions must preserve the SAME question state pointer")
			driver := subAgentDriver{call: template}
			woken := driver.callFor("p")
			require.Same(t, st, woken.batchQuestion,
				"subAgentDriver.callFor must preserve the SAME question state pointer")
			// The VALUES survive too: identical pointers would mask a
			// shallow-copy that swaps in a mutated clone.
			require.Equal(t, st.Depth, woken.batchQuestion.Depth)
			require.Equal(t, st.Route, woken.batchQuestion.Route)
			require.Equal(t, st.BindingOK, woken.batchQuestion.BindingOK)
		})
	}
	t.Run("stateless_stays_nil", func(t *testing.T) {
		// A non-batch call built from any constructor must stay state-less:
		// a fabricated state would arm a fake capability.
		template := SessionAgentCall{SessionID: "plain"}
		require.Nil(t, callFromActive(template).batchQuestion)
		require.Nil(t, newDrainCall(template).batchQuestion)
		require.Nil(t, newDrainCall(callFromActive(template)).batchQuestion)
		require.Nil(t, applyDrainModelOptions(template, Model{}).batchQuestion)
		require.Nil(t, (subAgentDriver{call: template}).callFor("").batchQuestion)
	})
}

// TestSessionAgentCallDataCarriesNoQuestionCapability: the durable queue
// serialization boundary, verified fail-closed. The durable queue never
// carries a capability-bearing call in the first place: the producers
// (restartOrphanedWithRetry, startDetachedRun, handleInterruptTick) refuse
// such calls with ErrBatchQuestionCapabilityNotDurable (batch_scope.go),
// so no row carrying or lacking the capability is ever replayed. This test
// pins the serialization: ToSessionAgentCallData must not smuggle the
// capability into session.SessionAgentCallData — the schema must be
// identical to a capability-less call's, and a rebuilt call (which is nil
// BY CONSTRUCTION for capability-bearing inputs, since those are refused
// before ever being persisted) must carry no state.
func TestSessionAgentCallDataCarriesNoQuestionCapability(t *testing.T) {
	capabilityLess := SessionAgentCall{SessionID: "child"}
	call := SessionAgentCall{SessionID: "child", batchQuestion: &batchChildQuestionState{
		Depth: 2,
		Route: false,
		Binding: batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
			return json.RawMessage(`"answered"`), nil
		}},
		BindingOK: true,
	}}
	keysOf := func(t *testing.T, data session.SessionAgentCallData) map[string]any {
		t.Helper()
		raw, err := json.Marshal(data)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		return m
	}
	t.Run("schema_identical_nothing_smuggled", func(t *testing.T) {
		withState := keysOf(t, ToSessionAgentCallData(call))
		baseline := keysOf(t, ToSessionAgentCallData(capabilityLess))
		require.Equal(t, len(baseline), len(withState),
			"the serialized schema must not grow when the call carries question capability")
		require.Equal(t, baseline, withState,
			"the serialized key sets must be IDENTICAL: the capability must not be smuggled into the durable row")
		for key := range withState {
			require.Contains(t, baseline, key,
				"no capability-specific key may appear in the durable row")
		}
	})
	t.Run("rebuild_carries_no_state", func(t *testing.T) {
		// Fail-closed by construction: the durable producers refuse
		// capability-bearing calls with ErrBatchQuestionCapabilityNotDurable,
		// so a row replayed from the durable queue never carried the
		// capability — round-tripping one through the serialization boundary
		// must therefore rebuild a nil capability, never a re-armed one.
		rebuilt, err := FromSessionAgentCallData(ToSessionAgentCallData(call))
		require.NoError(t, err)
		require.Nil(t, rebuilt.batchQuestion,
			"a rebuilt call must never carry question capability: it re-arms only from trusted intake")
	})
}
