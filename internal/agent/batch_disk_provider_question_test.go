package agent

// R12-H2 chunk 3b, acceptance item 8: a REAL agent-kind batch node whose
// pinned delegation leaf runs a REAL CHILD TURN with a SCRIPTED MODEL
// (review P1-7c): the leaf constructs a production sessionAgent
// (NewSessionAgent: SmartModel = the scripted model, Sessions/Messages
// services over the SAME SQLite store, IsYolo) and runs one SessionAgentCall
// on a child session created through the same session service. The scripted
// model drives the production question machinery: round 1 is an ask_question
// tool call on the PRODUCTION ask tool (tools.NewAskQuestionTool) wrapped by
// the REAL batch binding (buildBatchQuestionTools → batchAgentAskQuestionTool
// → binding.Ask → batchAgentExecution.askBatchQuestion → scope.hold →
// batchCoordinatorQuestionPublisher → session_notices), so the held question
// is published by production code and parked until the REAL
// jobbatch Runner.Inject delivers the answer at the held depth. Round 2 (the
// model, seeing the answer, decides to continue) calls the fs_write tool
// bound to the invocation's DiskProvider from the run context — the SECOND
// filesystem op in the SAME incarnation. Round 3 ends the child turn with
// final text.
//
// What is real: InstallBatchRuntime (real Runner + commit hook), the REAL
// batchExecutor agent family + batchCoordinatorAgentRuns driver (ingress
// leaf resolution, CallOptions/provider context stamping, question-binding
// and lifecycle stamping), the REAL batchCoordinatorQuestionPublisher, the
// REAL Runner.Inject acceptance, the REAL production ask tool + batch
// question wrapping, and a REAL sessionAgent turn. What is scripted: ONLY
// the model (a fantasy.LanguageModel fake emitting tool calls — model
// behavior is the thing fake BEHAVIOR, per design §3); the child fs_write
// is a leaf-level tool bound to opts.DiskProvider from the run context (the
// production fs_write family needs the coordinator permission service; the
// bound-OP oracle is identical: every write goes through the SAME
// DiskProvider instance and binding id).
//
// Oracles kept from the pre-rewrite version: the production publisher wrote
// the pending child_question notice carrying THIS incarnation's node id,
// nonzero token and depth 1; mid-hold the node stays live and the binding
// resolvable; the answer enters through the REAL Runner.Inject seam into
// the SAME incarnation; exactly ONE leaf incarnation and ONE root (no
// replacement batch/provider registration); both fs ops ran on the SAME
// provider instance with the SAME DiskBindingID; the exact OS targets are
// untouched (no OS fallback); the node settles completed with the answer;
// binding released only after dispose + the fixture's Construction release.
//
// Mutation caught: if the driver rebuilt the child context without the
// provider stamp (batch_install_agent_driver.go WithCallOptions /
// batchDiskProviderContextKey), op2 would resolve no bound provider — the
// child fs tool fails closed (and were it to fall back to the OS disk, the
// fake count would stay 1 and a real temp file would appear; both are
// asserted impossible here). A held-question implementation that answered
// by submitting a replacement batch or re-running the leaf would show a
// second incarnation/root.
//
// The Stop-while-held case (batch_held_question_inject_test.go
// TestStopWhileHeldKeepsQuestionAndRefusesLateInject) shares this harness
// unchanged: when the hold fails (stopped scope), the scripted model sees
// the failed ask and ends the child turn WITHOUT the second op, so Stop
// never runs op2 through the real child turn either.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// questionProviderLeaf is the pinned AGENT-kind delegation leaf. Run performs
// fs op #1 through the executor-acquired provider, checks the binding
// registry mid-incarnation, binds the child session (production lifecycle
// shape), then runs ONE REAL child turn: a sessionAgent with a SCRIPTED
// model whose round 1 is the PRODUCTION ask_question (parking the
// incarnation in the production held question) and whose round 2 performs
// fs op #2 through the SAME provider before the model finishes the turn.
type questionProviderLeaf struct {
	mu              sync.Mutex
	expectedBinding string
	childSession    string
	path1, path2    string

	t *testing.T
	// sessions/messages are the REAL session and message services over the
	// SAME SQLite store the batch rows live in; the child session is a real
	// row created through the service.
	sessions session.Service
	messages message.Service

	op1Provider, op2Provider tools.DiskProvider
	op1Binding, op2Binding   string
	midLookupOK              bool
	answer                   json.RawMessage
	// askFailed records that the production ask returned without an answer
	// (stopped/cancelled hold): the scripted model ends the turn WITHOUT
	// the second op (Stop-while-held shape).
	askFailed atomic.Bool
	runs      int
}

var _ fantasy.AgentTool = (*questionProviderLeaf)(nil)

func newQuestionProviderLeaf(t *testing.T, bindingID, childSession, path1, path2 string, sessions session.Service, messages message.Service) *questionProviderLeaf {
	return &questionProviderLeaf{
		t: t, expectedBinding: bindingID, childSession: childSession,
		path1: path1, path2: path2,
		sessions: sessions, messages: messages,
	}
}

func (l *questionProviderLeaf) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "agent", Description: "delegation leaf: provider write, scripted child turn with the production ask_question, provider write"}
}

func (l *questionProviderLeaf) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (l *questionProviderLeaf) SetProviderOptions(fantasy.ProviderOptions) {}

func (l *questionProviderLeaf) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if l.t != nil {
		l.t.Logf("q8 probe: leaf Run entry (binding=%s)", l.expectedBinding)
	}
	l.mu.Lock()
	l.runs++
	l.mu.Unlock()
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.DiskProvider == nil {
		return fantasy.NewTextResponse("q8 leaf: no bound disk provider in the run context"), errors.New("q8 leaf: no bound disk provider")
	}
	// fs op #1 through the executor-acquired provider.
	if err := opts.DiskProvider.WriteFile(ctx, l.path1, []byte("q8-op1"), 0o644); err != nil {
		return fantasy.NewTextResponse("q8 leaf: op1 failed: " + err.Error()), err
	}
	l.mu.Lock()
	l.op1Provider = opts.DiskProvider
	l.op1Binding = opts.DiskBindingID
	reg := batchDiskRegistryFromContext(ctx)
	l.mu.Unlock()
	// The binding stays resolvable BETWEEN op1 and op2 (the executor lease
	// and the invocation registry are still stamped on the run context).
	if reg != nil {
		_, err := reg.Lookup(l.expectedBinding)
		l.mu.Lock()
		l.midLookupOK = err == nil
		l.mu.Unlock()
	}
	if l.t != nil {
		l.t.Logf("q8 probe: leaf after op1 (midLookupOK=%v)", l.midLookupOK)
	}
	// Production lifecycle shape: bind the child session before the child
	// turn (mirrors the coordinator's runSubAgent binding via the
	// driver-stamped batchAgentLifecycle).
	if lifecycle, ok := ctx.Value(batchAgentLifecycleContextKey{}).(batchAgentLifecycle); ok && lifecycle.BindChildSession != nil {
		lifecycle.BindChildSession(l.childSession)
	}
	// The REAL child turn: a production sessionAgent over the SAME store
	// with a SCRIPTED model (fake model BEHAVIOR only — the turn, the tool
	// execution and the ask machinery are production code).
	model := &questionChildModel{leaf: l}
	// The PRODUCTION ask tool, wrapped by the REAL batch question binding
	// (buildBatchQuestionTools → batchAgentAskQuestionTool → binding.Ask).
	// The recording wrapper captures the answer envelope for the oracle and
	// flags a failed ask for the scripted model.
	wrappedAsk, werr := buildBatchQuestionTools([]fantasy.AgentTool{tools.NewAskQuestionTool()}, ctx)
	if l.t != nil {
		l.t.Logf("q8 probe: buildBatchQuestionTools -> %d tools (err=%v)", len(wrappedAsk), werr)
	}
	childTools := []fantasy.AgentTool{
		&recordingAskTool{leaf: l, inner: batchMustWrapAsk(wrappedAsk, werr, l.t)},
		// fs op #2: the model's write tool, bound to the SAME provider from
		// the run context (no OS fallback — a nil provider fails closed).
		&questionChildFSTool{leaf: l, path: l.path2},
	}
	child := NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:  Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		Sessions:   l.sessions,
		Messages:   l.messages,
		Tools:      childTools,
	})
	if sa, ok := child.(*sessionAgent); ok {
		// Registered after the harness cleanups, so (LIFO) it runs first.
		l.t.Cleanup(sa.runWg.Wait)
	}
	// The child inherits the leaf's run context: CallOptions (provider +
	// immutable binding handle), question depth and the batch lifecycle all
	// travel with it — this is the stamp the mutation oracle watches.
	if l.t != nil {
		_, hasBinding := batchAgentQuestionBindingFrom(ctx)
		l.t.Logf("q8 probe: child Run start (question binding in ctx=%v)", hasBinding)
	}
	res, err := child.Run(ctx, SessionAgentCall{SessionID: l.childSession, Prompt: "continue the delegation"})
	if l.t != nil {
		finish := "<nil result>"
		if res != nil {
			finish = string(res.Response.FinishReason)
		}
		l.t.Logf("q8 probe: child Run returned (err=%v finish=%s)", err, finish)
	}
	if err != nil {
		return fantasy.NewTextResponse("q8 leaf: child turn failed: " + err.Error()), err
	}
	if res == nil || res.Response.FinishReason != fantasy.FinishReasonStop {
		return fantasy.NewTextResponse("q8 leaf: child turn did not finish with a stop"), errors.New("q8 leaf: child turn not finished")
	}
	l.mu.Lock()
	answer := l.answer
	l.mu.Unlock()
	// fs op #2 already ran INSIDE the child turn (round 2), through the
	// SAME provider instance and binding — recorded by questionChildFSTool.
	return fantasy.NewTextResponse("q8 done, answer=" + string(answer)), nil
}

// batchMustWrapAsk applies the production question wrapping and requires the
// ask tool to have been wrapped (the question binding is stamped on the
// driver context; a missing wrap is the mutation).
func batchMustWrapAsk(list []fantasy.AgentTool, err error, t *testing.T) fantasy.AgentTool {
	t.Helper()
	require.NoError(t, err, "the production question wrapping must install")
	for _, tool := range list {
		if tool.Info().Name == tools.AskQuestionToolName {
			return tool
		}
	}
	t.Fatalf("buildBatchQuestionTools did not wrap the ask tool: the question binding is missing from the driver context")
	return nil
}

// recordingAskTool wraps the production-wrapped ask tool, records the answer
// envelope for the oracle and flags a failed (stopped/cancelled) ask.
type recordingAskTool struct {
	leaf  *questionProviderLeaf
	inner fantasy.AgentTool
}

func (t *recordingAskTool) Info() fantasy.ToolInfo { return t.inner.Info() }
func (t *recordingAskTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *recordingAskTool) SetProviderOptions(p fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(p)
}

func (t *recordingAskTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if t.leaf.t != nil {
		t.leaf.t.Logf("q8 probe: ask tool executing (input=%s)", call.Input)
	}
	resp, err := t.inner.Run(ctx, call)
	if err != nil || resp.IsError {
		t.leaf.askFailed.Store(true)
	} else {
		t.leaf.mu.Lock()
		t.leaf.answer = json.RawMessage(resp.Content)
		t.leaf.mu.Unlock()
	}
	return resp, err
}

// questionChildModel is the SCRIPTED model of the child turn (the ONLY fake
// in the flow — model behavior, per design §3). Round 1 asks; round 2 writes
// (only if the ask was ANSWERED); round 3 finishes with text.
type questionChildModel struct {
	leaf *questionProviderLeaf
	step atomic.Int32
	reqs atomic.Int32
}

func (m *questionChildModel) Provider() string { return "q8-scripted" }
func (m *questionChildModel) Model() string    { return "q8-scripted-model" }

func (m *questionChildModel) streamToolCall(id, name, input string) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{
			Type: fantasy.StreamPartTypeToolCall, ID: id,
			ToolCallName: name, ToolCallInput: input,
		}) {
			return
		}
		if m.leaf != nil && m.leaf.t != nil {
			m.leaf.t.Logf("q8 probe: scripted model emitted tool call %s (id=%s)", name, id)
		}
		yield(fantasy.StreamPart{
			Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls,
			Usage: fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}
}

func (m *questionChildModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	n := m.reqs.Add(1)
	if leaf := m.leaf; leaf != nil && leaf.t != nil {
		leaf.t.Logf("q8 probe: scripted model Stream #%d (tools=%d prompt=%q)", n, len(call.Tools), promptText(call.Prompt))
	}
	// The turn's title-generation goroutine (startTitleGeneration →
	// generateTitle) shares this agent's models and issues its own blocking
	// Stream calls concurrently with the turn, WITHOUT tools. Those requests
	// must not consume the turn's scripted rounds — a naive step counter let
	// the title attempts eat rounds 1 (the ask tool call) and 2, so the turn
	// itself never executed the ask and the held question was never
	// published. Any request carrying no tool definitions is therefore a
	// side-channel call (title generation): answer it with a bare stop and
	// leave the script untouched. The turn's own requests always carry the
	// agent's tools (the wrapped ask + fs_write).
	if len(call.Tools) == 0 {
		return questionStopStream(), nil
	}
	switch m.step.Add(1) {
	case 1: // round 1: the PRODUCTION ask_question tool call.
		return m.streamToolCall("call-q8-ask", tools.AskQuestionToolName, `{"question":"continue?"}`), nil
	case 2: // round 2: the answer arrived → the model continues and writes.
		if m.leaf.askFailed.Load() {
			// The hold failed (stopped/cancelled): the model ends the turn
			// without the second op (Stop-while-held shape).
			return questionStopStream(), nil
		}
		return m.streamToolCall("call-q8-write", "fs_write", `{"path":"`+m.leaf.path2+`"}`), nil
	default: // round 3: turn complete.
		return questionStopStream(), nil
	}
}

// promptText flattens a fantasy prompt for the q8 probes (diagnostics only).
func promptText(p fantasy.Prompt) string {
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Sprintf("%+v", p)
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

func questionStopStream() fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop,
			Usage: fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}
}

func (m *questionChildModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	stream, err := m.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	resp := &fantasy.Response{}
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeFinish {
			resp.FinishReason = part.FinishReason
		}
	}
	return resp, nil
}

func (m *questionChildModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("q8 scripted model: objects not implemented")
}

func (m *questionChildModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("q8 scripted model: object streams not implemented")
}

// questionChildFSTool is the child turn's write tool: it resolves the bound
// provider from the run context (the driver's provider stamp) and performs
// fs op #2 on the SAME provider instance — never OS disk, never a fallback.
type questionChildFSTool struct {
	leaf *questionProviderLeaf
	path string
}

func (t *questionChildFSTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "fs_write", Description: "child write through the bound provider"}
}
func (t *questionChildFSTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *questionChildFSTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *questionChildFSTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.DiskProvider == nil {
		return fantasy.NewTextResponse("q8 child: no bound disk provider"), errors.New("q8 child: no bound disk provider")
	}
	if err := opts.DiskProvider.WriteFile(ctx, t.path, []byte("q8-op2"), 0o644); err != nil {
		return fantasy.NewTextResponse("q8 child: op2 failed: " + err.Error()), err
	}
	t.leaf.mu.Lock()
	t.leaf.op2Provider = opts.DiskProvider
	t.leaf.op2Binding = opts.DiskBindingID
	t.leaf.mu.Unlock()
	return fantasy.NewTextResponse("q8 child wrote " + filepath.ToSlash(t.path)), nil
}

// questionInstall is one real install harness for the agent-kind provider
// question flow. It mirrors newLifecycleInstall but pins an AGENT leaf,
// wires the REAL coordinator driver (batchCoordinatorAgentRuns) with the
// REAL live descendant reader, and stamps the executor env's Ingress with
// the App-owned ingress (see the file header for why). The session/message
// services over the SAME store let the pinned leaf run a REAL child turn.
type questionInstall struct {
	t           *testing.T
	store       *session.BatchStore
	conn        *sql.DB
	sessions    session.Service
	messages    message.Service
	install     *BatchInstall
	leaf        *questionProviderLeaf
	fake        *lifecycleFakeDisk
	bindingID   string
	registryID  string
	fingerprint string
	realDir     string
	// path1/path2 are the EXACT absolute OS paths the leaf writes to,
	// preseeded with sentinel content; the untouched-disk assertions inspect
	// these actual targets, never an unrelated temp dir.
	path1, path2        string
	releaseConstruction func()
	sessionID           string
}

func newQuestionInstall(t *testing.T) *questionInstall {
	t.Helper()
	store, conn, _ := newTestBatchStore(t)
	bindings := NewDiskProviderRegistry()
	fake := newLifecycleFakeDisk()
	bindingID, err := bindings.RegisterProvider(fake)
	require.NoError(t, err)
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	childSess, err := sessions.Create(context.Background(), "q8 child")
	require.NoError(t, err, "the child session row exists")
	childSession := childSess.ID
	// The EXACT absolute OS targets, preseeded with sentinel content: if the
	// driver fell back to the real disk, these files would change.
	realDir := t.TempDir()
	path1 := filepath.ToSlash(filepath.Join(realDir, "op1.txt"))
	path2 := filepath.ToSlash(filepath.Join(realDir, "op2.txt"))
	require.NoError(t, os.WriteFile(filepath.FromSlash(path1), []byte("q8-sentinel-1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.FromSlash(path2), []byte("q8-sentinel-2"), 0o644))
	leaf := newQuestionProviderLeaf(t, bindingID, childSession, path1, path2, sessions, messages)
	fingerprint := batchToolsetFingerprint([]fantasy.AgentTool{leaf})
	registryID := "q8-reg-" + uuid.NewString()[:8]
	registries := NewBatchLeafRegistryStore()
	require.NoError(t, registries.Register(registryID, &policyLeafRegistry{
		entries: map[string]policyLeafEntry{"agent": {layers: []string{"tool"}, tool: leaf}},
	}))
	// R12-H2 §1.2: the executor's Execution owner names the pinned
	// registry's construction record; hold its Construction owner exactly as
	// the trusted call state's build scope does (released explicitly before
	// the final unavailability assertion).
	releaseConstruction := pinTestConstruction(t, bindings, registryID)
	// The REAL store-backed live descendant reader; the host probe is
	// overridden because a unit test holds no real cross-process host lock.
	live := session.NewBatchLiveReader(db.New(conn), t.TempDir())
	live.IsOwnHost = func(string) bool { return false }
	live.ProbeHost = func(string, string) (session.HostLockStatus, error) {
		return session.HostStatusAlive, nil
	}
	agentDeps := batchAgentDeps{
		Runs:              batchCoordinatorAgentRuns{},
		Descendants:       batchCoordinatorDescendants{live: live},
		QuestionPublisher: batchCoordinatorQuestionPublisher{store: store},
	}
	executor := newBatchExecutor(
		batchLeafEnv{
			Leaves:       registries.Resolve,
			DiskBindings: bindings,
		},
		newBatchExecutorFamilies(agentDeps),
	)
	deps := installTestDeps(t, store, conn, "unused")
	deps.Executor = executor
	deps.AgentDeps = agentDeps
	deps.DiskBindings = bindings
	deps.Registries = registries
	deps.DonePoll = time.Millisecond
	install, err := InstallBatchRuntime(context.Background(), deps)
	require.NoError(t, err)
	t.Cleanup(func() {
		if install.Runner != nil {
			require.NoError(t, install.Runner.Close(context.Background()))
		}
	})
	// R12-H2 P1-4 executor-env contract: the App-owned ingress reaches the
	// agent driver through env.Ingress (batch_exec_agent.go start, via
	// callIngress). Production wiring binds the install ingress here the
	// same way app_batch_install.go does.
	ingress := &batchWorkIngress{rt: install.Runtime, registries: registries, diskBindings: bindings}
	executor.env.Ingress = func() BatchWorkIngress { return ingress }
	BindRuntimeIngress(install.Runtime, ingress)
	return &questionInstall{
		t: t, store: store, conn: conn, sessions: sessions, messages: messages,
		install: install, leaf: leaf,
		fake: fake, bindingID: bindingID, registryID: registryID,
		fingerprint: fingerprint, realDir: realDir,
		path1: path1, path2: path2, releaseConstruction: releaseConstruction,
		sessionID: "sess-owner-q8-" + uuid.NewString()[:8],
	}
}

// submitAgentRoot submits the one-item AGENT root through the real runner
// handle: the payload pins the registry identity, the restricted toolset
// fingerprint and the provider binding id, so the executor acquires the
// binding lease and the agent family stamps the provider onto the
// incarnation's run context.
func (env *questionInstall) submitAgentRoot() jobbatch.ID {
	env.t.Helper()
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "call-q8", SessionID: env.sessionID,
		Input: json.RawMessage(`{}`), RegistryID: env.registryID,
		Policy: &batchLeafPolicyContext{
			RegistryID: env.registryID, RestrictedToolset: env.fingerprint,
			DiskProvider: true, DiskBindingID: env.bindingID,
		},
	})
	require.NoError(env.t, err)
	root := jobbatch.ID("root-q8-" + uuid.NewString()[:8])
	_, err = env.install.RunnerHandle.Submit(context.Background(), jobbatch.SubmitRequest{
		Root: root, Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}},
		Ref: "call-q8",
	})
	require.NoError(env.t, err, "the agent root is admitted")
	return root
}

func TestBatchAgentQuestionHoldsScopeThenSecondProviderOp(t *testing.T) {
	t.Parallel()
	env := newQuestionInstall(t)
	root := env.submitAgentRoot()

	// FIRST real assertion (while the child turn is parked in the
	// production ask): the production publisher (batchAgentAskQuestionTool →
	// binding.Ask → askBatchQuestion → publishQuestion →
	// batchCoordinatorQuestionPublisher.PublishChildQuestion →
	// store.BatchPublishQuestion) wrote the pending child_question notice
	// carrying THIS incarnation's node id, nonzero token and depth 1.
	var text string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&text) == nil
	}, 5*time.Second, 5*time.Millisecond, "the production publisher wrote the held question")
	node, tok, ok := parseBatchQuestionMarker(text)
	require.True(t, ok, "the notice carries the production marker: %q", text)
	require.NotEmpty(t, node)
	require.Greater(t, tok, int64(0), "the incarnation token is nonzero")
	require.Contains(t, text, "depth=1", "the marker stamps the held depth")
	require.Contains(t, text, "continue?", "the question body follows the marker (asked by the scripted child model)")

	// Mid-hold: exactly one provider write so far (round 1 of the child
	// turn asked BEFORE any second op), no real-disk file, the node still
	// live (a held question keeps the incarnation open), and the binding
	// still resolvable.
	require.Equal(t, 1, env.fake.writeCount(), "op1 landed before the answer")
	_, hasOp1 := env.fake.has(env.path1)
	require.True(t, hasOp1)
	sentinel, err := os.ReadFile(filepath.FromSlash(env.path1))
	require.NoError(t, err)
	require.Equal(t, "q8-sentinel-1", string(sentinel),
		"the exact OS target op1 wrote to must be untouched while held (no OS fallback)")
	_, err = env.install.DiskBindings.Lookup(env.bindingID)
	require.NoError(t, err, "the binding stays resolvable while the question is held")
	var state string
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		string(root), env.sessionID).Scan(&state))
	require.Equal(t, "running", state, "the held question keeps the node live")

	// The answer enters through the REAL Runner.Inject seam (the production
	// inject_agent path): accepted into the SAME incarnation. The parked
	// child turn — the REAL sessionAgent parked inside the production ask
	// tool — wakes with the answer.
	runner := env.install.RunnerHandle.runner
	require.NoError(t, runner.Inject(context.Background(), jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"text":"yes, proceed"}`)),
		"the answer is accepted into the parked incarnation")

	// The child turn continued (round 2 wrote op2 on the SAME provider,
	// round 3 finished), and the node settles completed once the scope
	// confirms closure (descendants read).
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			string(root), env.sessionID).Scan(&state) == nil && state == "completed"
	}, 10*time.Second, 5*time.Millisecond, "the answered incarnation settles completed")

	op1, op2, b1, b2, midOK, answer := env.leaf.snapshot()
	require.True(t, midOK, "the binding was resolvable between op1 and op2")
	require.True(t, op1 == tools.DiskProvider(env.fake), "op1 ran on the custom provider instance")
	require.True(t, op2 == op1, "op2 ran on the SAME provider instance as op1, inside the SAME incarnation")
	require.Equal(t, env.bindingID, b1, "op1 carried the pinned DiskBindingID")
	require.Equal(t, b1, b2, "op2 carried the SAME DiskBindingID (the child turn inherited the provider stamp)")
	require.Equal(t, "yes, proceed", string(answer), "the injected answer reached the production ask tool")
	require.Equal(t, 2, env.fake.writeCount(), "both ops ran on the custom fake")
	got1, ok1 := env.fake.has(env.path1)
	got2, ok2 := env.fake.has(env.path2)
	require.True(t, ok1 && got1 == "q8-op1", "op1 content is on the custom provider")
	require.True(t, ok2 && got2 == "q8-op2", "op2 content is on the custom provider")
	for p, want := range map[string]string{env.path1: "q8-sentinel-1", env.path2: "q8-sentinel-2"} {
		sentinel, err := os.ReadFile(filepath.FromSlash(p))
		require.NoError(t, err)
		require.Equal(t, want, string(sentinel), "the exact OS target %s was never touched by an OS fallback", p)
	}
	require.Equal(t, 1, env.leaf.runCount(), "exactly one leaf incarnation for the whole flow")
	// No replacement batch: exactly ONE root for the owner.
	var roots int
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?1 AND kind = 'batch' AND parent_id IS NULL`,
		env.sessionID).Scan(&roots))
	require.Equal(t, 1, roots, "no replacement batch was submitted")
	// The child session really ran a turn: its messages live in the same
	// store (user prompt + assistant rows from the scripted-model turn).
	var childMsgs int
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM messages WHERE session_id = ?1`, env.leaf.childSession).Scan(&childMsgs))
	require.Greater(t, childMsgs, 0, "the REAL child turn persisted messages for the child session")

	// A second answer on the now-terminal incarnation is refused (same
	// deterministic rejection the production Inject path guarantees).
	require.ErrorIs(t, runner.Inject(context.Background(), jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"text":"late"}`)), jobbatch.ErrTerminal)

	// The root settled completed: the engine disposes its Execution owner
	// only on Runner.Release (root is the submitAgentRoot return).
	require.NoError(t, env.install.Runner.Release(context.Background(), root))

	// After settle + dispose the owners are gone: release THIS fixture's own
	// Construction owner too, then the binding's owner set is empty.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding unavailable after dispose")
}

// runCount reads the leaf's incarnation counter.
func (l *questionProviderLeaf) runCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runs
}

func (l *questionProviderLeaf) snapshot() (op1, op2 tools.DiskProvider, b1, b2 string, midOK bool, answer json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.op1Provider, l.op2Provider, l.op1Binding, l.op2Binding, l.midLookupOK, l.answer
}
