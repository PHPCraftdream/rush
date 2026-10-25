package agent

// R12-H2a round 2 regression test: pinCallTools' fail path must release the
// construction owners buildTools already pinned into the CALL's build
// scope. buildTools succeeds (batch runtime binding, working controls
// stub), the injected ready-gate task fails pinCallTools' OWN
// readyWg.Wait() drain via pinCallToolsReadyGateSeam (the deterministic
// mechanism from r5_1_pincalltools_failclosed_test.go), and the fail
// closure closes the call's scope. Oracle: the disk capability manager's
// owner set returns to its pre-build count, the pinned registry identity
// was dropped from the leaf index, and it no longer resolves. Mirrors
// Variant B of TestConstructionScopeBoundary (which exercises the same
// release on finishToolAssembly's own failure path) but through
// pinCallTools' ready-gate failure branch.

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// workingModelControls is a BatchModelControl stub whose install step
// SUCCEEDS (with no control tools), so the pinCallTools failure under test
// comes from the ready gate, not from the controls.
type workingModelControls struct{}

func (workingModelControls) Run(context.Context, BatchInvocation, *policyLeafRegistry, batchModelSpec) (jobbatch.ID, error) {
	return jobbatch.ID(""), nil
}

func (workingModelControls) Add(context.Context, string, string, string, BatchInvocation, *policyLeafRegistry, []batchModelItem) ([]jobbatch.ID, error) {
	return nil, nil
}

func (workingModelControls) StatusNode(context.Context, string, string, string) (any, error) {
	return nil, nil
}

func (workingModelControls) Output(context.Context, string, string, string, uint64) (any, error) {
	return nil, nil
}

func (workingModelControls) Stop(context.Context, string, string, string) (any, error) {
	return nil, nil
}

func (workingModelControls) InstallBatchModelControls(*BatchLeafRegistry, BatchInvocation) ([]fantasy.AgentTool, error) {
	return nil, nil
}

func TestPinCallToolsReadyGateErrorReleasesConstruction(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	// Light in-package batch binding (batch_construction_scope_boundary_test.go
	// shape): the build REALLY pins construction owners, and the working
	// controls stub keeps the assembly alive past the pin so the failure is
	// the ready gate's, not the controls'.
	caps := NewDiskProviderRegistry()
	registries := NewBatchLeafRegistryStore()
	caps.setRegistryDrop(registries.Delete)
	bind := &batchCoordinatorBinding{
		ingress:      &batchWorkIngress{registries: registries, diskBindings: caps},
		controls:     workingModelControls{},
		diskBindings: caps,
	}
	coordinatorBatchBindings.Store(coord, bind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(coord) })

	// Trusted call state carrying its OWN build scope; DisableSubAgents
	// makes the toolset distinct (scopedCallOptionsRequireDistinctTools), so
	// the ready-gate failure returns a non-nil error through the fail
	// closure instead of the legacy log-only (nil, nil) shape.
	callOpts := &CallOptions{DisableSubAgents: true}
	scope := EnsureCallBuildScope(callOpts)
	require.NotNil(t, scope, "the trusted call state carries its own build scope")
	ctx := WithCallOptions(batchCallCtx("round2-leak"), callOpts)

	dropped := make(map[string]struct{}, 1)
	caps.setRegistryDrop(func(id string) {
		dropped[id] = struct{}{}
		registries.Delete(id)
	})
	pre := caps.ownerCount()

	armReadyGateSeam(t, coord)
	_, err := coord.pinCallTools(ctx, coord.cfg.Config())
	caps.setRegistryDrop(registries.Delete)

	require.Error(t, err, "the ready-gate drain failure must fail pinCallTools closed")
	require.Contains(t, err.Error(), errSimulatedReadyGateFailure.Error(),
		"the failure must be the injected ready-gate error wrapped through the fail closure")

	// THE ORACLE: the construction buildTools pinned is gone.
	require.Equal(t, pre, caps.ownerCount(),
		"the failed pinCallTools must release the construction owner: owner set back to the pre-build count")
	require.NotEmpty(t, dropped, "the pinned registry identity was dropped from the index")
	for id := range dropped {
		_, resolveErr := registries.Resolve(id)
		require.Error(t, resolveErr, "the pinned registry identity of the failed build must no longer resolve")
	}
}

// ---------------------------------------------------------------------------
// R12-H2a round 2, part 2: full-path construction release on the public
// entry points and own-scope queued calls.
// ---------------------------------------------------------------------------

// round2FakeDisk is a DiskProvider for call-state binding: it embeds the
// real OSDisk so any incidental build-time path resolution behaves like the
// host filesystem, while the scripted turns under test make NO tool calls —
// no provider traffic of our own ever happens.
type round2FakeDisk struct {
	tools.DiskProvider
}

var _ tools.DiskProvider = round2FakeDisk{}

// round2LiveHarness is the full-run coordinator harness for the round-2
// part-2 tests: a struct-literal coordinator built like
// newToolPinningCoordinator (all services buildTools needs plus the coder
// prompt resolveSessionModels needs), with a REAL sessionAgent as
// currentAgent whose smart/fast slots resolve to local single-turn SSE
// servers — so coord.Run/RunWithReservedOwnership/InterruptAndSend drive
// real turns end-to-end without a network. The light batch binding is
// stored exactly like the direct pinCallTools test above, so resolution's
// pinCallTools REALLY pins a construction owner into the call's build
// scope.
type round2LiveHarness struct {
	coord      *coordinator
	sa         *sessionAgent
	caps       *diskCapabilityManager
	registries *BatchLeafRegistryStore
	sess       fakeEnv
}

func newRound2LiveHarness(t *testing.T, smartSrv, fastSrv *httptest.Server) *round2LiveHarness {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	register := func(providerID, baseURL string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:      providerID,
			Type:    openai.Name,
			BaseURL: baseURL,
			Models: []catwalk.Model{
				{ID: "round2-probe", Name: "Round2 Probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: "round2-probe"}
	}
	cfg.Config().Models[config.SelectedModelTypeSmart] = register("round2-smart", smartSrv.URL)
	cfg.Config().Models[config.SelectedModelTypeFast] = register("round2-fast", fastSrv.URL)
	cfg.SetupAgents()

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		prompt:      p,
		modelCache:  csync.NewMap[string, cachedModelPair](),
	}

	lmFrom := func(baseURL string) fantasy.LanguageModel {
		provider, err := openaicompat.New(
			openaicompat.WithBaseURL(baseURL),
			openaicompat.WithAPIKey("probe"),
		)
		require.NoError(t, err)
		lm, err := provider.LanguageModel(context.Background(), "probe")
		require.NoError(t, err)
		return lm
	}
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{
			Model:      lmFrom(smartSrv.URL),
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
		FastModel:            Model{Model: lmFrom(fastSrv.URL), CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "round2 probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        t.TempDir(),
	}).(*sessionAgent)
	coord.currentAgent = sa

	caps := NewDiskProviderRegistry()
	registries := NewBatchLeafRegistryStore()
	caps.setRegistryDrop(registries.Delete)
	bind := &batchCoordinatorBinding{
		ingress:      &batchWorkIngress{registries: registries, diskBindings: caps},
		controls:     workingModelControls{},
		diskBindings: caps,
	}
	coordinatorBatchBindings.Store(coord, bind)
	t.Cleanup(func() { coordinatorBatchBindings.Delete(coord) })

	return &round2LiveHarness{coord: coord, sa: sa, caps: caps, registries: registries, sess: env}
}

// countDrop swaps in a counting registry-drop hook and restores the plain
// one on cleanup, mirroring the direct pinCallTools test above.
func (h *round2LiveHarness) countDrop(t *testing.T) map[string]struct{} {
	t.Helper()
	dropped := make(map[string]struct{}, 1)
	h.caps.setRegistryDrop(func(id string) {
		dropped[id] = struct{}{}
		h.registries.Delete(id)
	})
	t.Cleanup(func() { h.caps.setRegistryDrop(h.registries.Delete) })
	return dropped
}

// TestRunWithReservedOwnershipReleasesConstruction drives the REAL public
// coord.RunWithReservedOwnership (the rerun/reviewer reserved path) to a
// full, successful, tool-call-free turn against a real sessionAgent backed
// by a local single-turn SSE server.
//
// Production mechanism under test: the R12-H2a round-2 scope-close defer in
// coordinator_run.go RunWithReservedOwnership — resolution
// (resolveSessionModels -> pinCallTools, with the batch runtime binding
// stored above) builds this call's coder toolset and pins a construction
// owner into the trusted call state's build-resource scope; every return
// from RunWithReservedOwnership (this function returns only after the
// whole reserved run completes) must close that scope, releasing exactly
// the run's own constructions.
//
// Mutation caught: removing the RunWithReservedOwnership defer leaks the
// construction owner (and its provider-capturing toolset registry) past
// the run — the owner-count oracle below goes red.
func TestRunWithReservedOwnershipReleasesConstruction(t *testing.T) {
	var calls atomic.Int64
	srv := singleTurnSSEServer(&calls)
	t.Cleanup(srv.Close)
	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	h := newRound2LiveHarness(t, srv, titleSrv)
	dropped := h.countDrop(t)

	sess, err := h.sess.sessions.Create(t.Context(), "round2 reserved run")
	require.NoError(t, err)

	// Trusted call state bound to the SAME caps manager: the Call owner is
	// the only owner before the run.
	opts, release, err := BindCallDiskProvider(h.caps, round2FakeDisk{}, "round2-reserved", nil)
	require.NoError(t, err)
	defer release()
	ctx := WithCallOptions(t.Context(), opts)
	pre := h.caps.ownerCount()
	require.Equal(t, 1, pre, "precondition: only the Call owner is held before the run")

	holdCtx, epoch, cancel, ok := h.coord.ReserveExclusive(ctx, sess.ID)
	require.True(t, ok, "ReserveExclusive must succeed on an idle session")
	defer cancel()
	_ = holdCtx

	_, err = h.coord.RunWithReservedOwnership(ctx, sess.ID, "round2 reserved run", epoch, cancel, nil, nil, nil)
	require.NoError(t, err, "the scripted single-turn model completes the reserved run without tool calls")

	// THE ORACLE: construction released at return — only the Call owner
	// remains.
	require.Equal(t, pre, h.caps.ownerCount(),
		"the reserved run must release its construction scope at return: only the Call owner remains")
	require.NotEmpty(t, dropped, "the run's pinned registry identity was dropped from the leaf index")
	for id := range dropped {
		_, resolveErr := h.registries.Resolve(id)
		require.Error(t, resolveErr, "the run's pinned registry identity must no longer resolve after the scope close")
	}
	require.Positive(t, calls.Load(), "the reserved turn must have reached the provider")
}

// TestInterruptAndSendQueuedCallOwnsBuildScope proves a queued call holds
// its OWN build-resource scope.
//
// Production mechanism under test: the R12-H2a round-2 clone in
// coordinator_interrupt.go InterruptAndSend (and the InjectAgent /
// interrupt-tick clones) — buildCall copies the interrupting ctx's
// CallOptions (and with them the interrupting turn's build scope) onto the
// queued call; the clone swaps in a fresh scope so the originating run's
// scope close can never fail-closed the queued call's later tool builds,
// and RunSessionAgentCall's own close releases exactly the queued call's
// constructions.
//
// Mutations caught: (a) reverting the clone to sharing the CallOptions
// pointer makes the stored replacement carry the ORIGINAL scope — the
// scope-identity oracle goes red; (b) with shared scopes, closing the
// original scope before the replacement runs fails the replacement closed
// (its later tool builds refuse against an already-closed scope) — the
// fail-closed-direction oracles go red.
func TestInterruptAndSendQueuedCallOwnsBuildScope(t *testing.T) {
	var calls atomic.Int64
	srv := singleTurnSSEServer(&calls)
	t.Cleanup(srv.Close)
	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	h := newRound2LiveHarness(t, srv, titleSrv)
	env := h.sess

	// Phase 1 — scope identity, deterministic: reserve the session so the
	// mailbox is owned but has NO live generation (mb.current.cancel nil).
	// The REAL coord.InterruptAndSend then takes the busy branch,
	// InterruptAndReplace records mb.replacement and has no cancel to fire,
	// so the replacement stays parked and observable without any race.
	sessParked, err := env.sessions.Create(t.Context(), "round2 queued scope, parked")
	require.NoError(t, err)

	holdCtx, _, holdCancel, ok := h.coord.ReserveExclusive(t.Context(), sessParked.ID)
	_ = holdCtx
	require.True(t, ok)
	defer holdCancel()

	parkOpts, parkRelease, err := BindCallDiskProvider(h.caps, round2FakeDisk{DiskProvider: tools.OSDisk()}, "round2-queued-parked", nil)
	require.NoError(t, err)
	defer parkRelease()
	parkCtx := WithCallOptions(t.Context(), parkOpts)
	originalScope := parkOpts.buildScope
	require.NotNil(t, originalScope)

	require.NoError(t, h.coord.InterruptAndSend(parkCtx, sessParked.ID, "parked replacement prompt", nil, nil),
		"InterruptAndSend must accept the interrupt against the owned (reserved) session")

	mb := h.sa.getMailbox(sessParked.ID)
	require.NotNil(t, mb.replacement, "the busy branch must record the call as the mailbox replacement")
	queued := *mb.replacement
	queuedTools := queued.Tools
	require.NotEmpty(t, queuedTools, "the queued call must carry its resolved pinned toolset")
	require.NotNil(t, queued.CallOptions, "the queued call must carry call options")
	require.NotEqual(t, originalScope, queued.CallOptions.buildScope,
		"the queued replacement call must carry its OWN build scope, not the interrupting ctx's shared one")

	registryIDs := make([]string, 0, len(queuedTools))
	for _, armed := range queuedTools {
		if ingress, ok := armed.(*batchIngressTool); ok {
			registryIDs = append(registryIDs, ingress.inv.RegistryID)
		}
	}
	require.NotEmpty(t, registryIDs, "queued toolset must include batch-armed tools with actual registry identities")
	// Closing the caller scope must leave each queued construction registered.
	originalScope.close()
	for _, registryID := range registryIDs {
		registry, resolveErr := h.registries.Resolve(registryID)
		require.NoError(t, resolveErr, "queued registry remains resolvable after caller scope close")
		for name := range registry.entries {
			if tool, toolErr := registry.resolve(name); toolErr == nil && tool != nil {
				break
			}
		}
		var tool fantasy.AgentTool
		for name := range registry.entries {
			if resolved, toolErr := registry.resolve(name); toolErr == nil {
				tool = resolved
				break
			}
		}
		require.NotNil(t, tool, "queued registry resolves an offered pinned tool")
	}
	queuedScope := queued.CallOptions.buildScope
	require.False(t, queuedScope.closed, "the queued call's own scope remains open")

	// Phase 2 — fail-closed direction on a LIVE busy turn: run the
	// original turn for real (blocked mid-stream on the SSE server's
	// proceed gate), interrupt it with a provider-bound ctx, close the
	// ORIGINAL scope BEFORE the replacement can construct anything, and
	// require the replacement to still construct and complete successfully.
	sessLive, err := env.sessions.Create(t.Context(), "round2 queued scope, live")
	require.NoError(t, err)

	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	liveSrv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(liveSrv.Close)
	// The live harness resolves models from config, so repoint the smart
	// provider at the gated server for this session's coordinator.
	liveCoord := h.coord
	liveCfg := liveCoord.cfg
	liveCfg.Config().Providers.Set("round2-smart", config.ProviderConfig{
		ID:      "round2-smart",
		Type:    openai.Name,
		BaseURL: liveSrv.URL,
		Models: []catwalk.Model{
			{ID: "round2-probe", Name: "Round2 Probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	liveCoord.modelCache = csync.NewMap[string, cachedModelPair]()

	opts, release, err := BindCallDiskProvider(h.caps, round2FakeDisk{}, "round2-queued-live", nil)
	require.NoError(t, err)
	defer release()
	interruptCtx := WithCallOptions(t.Context(), opts)
	liveOriginalScope := opts.buildScope

	runDone := make(chan error, 1)
	go func() {
		_, runErr := liveCoord.Run(t.Context(), sessLive.ID, "original busy turn, about to be replaced")
		runDone <- runErr
	}()
	select {
	case <-requestStarted:
	case r := <-runDone:
		t.Fatalf("Run returned before reaching the provider (err=%v)", r)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the original turn to reach the provider")
	}

	require.NoError(t, liveCoord.InterruptAndSend(interruptCtx, sessLive.ID, "live replacement prompt", nil, nil))
	// Capture the parked replacement's OWN clone scope before unblocking:
	// this is the scope the run loop must close when the replacement's
	// turn finishes (the R12-H2a round-2 run-loop close).
	liveMB := h.sa.getMailbox(sessLive.ID)
	liveMB.mu.Lock()
	require.NotNil(t, liveMB.replacement, "the live interrupt must have parked its replacement")
	require.NotNil(t, liveMB.replacement.CallOptions, "the parked replacement must carry call options")
	cloneScope := liveMB.replacement.CallOptions.buildScope
	liveMB.mu.Unlock()
	require.NotNil(t, cloneScope, "the replacement's clone must carry its own build scope")
	require.NotEqual(t, liveOriginalScope, cloneScope,
		"the replacement's clone scope must not be the interrupting ctx's shared one")
	// Close the ORIGINAL scope BEFORE letting the replacement run: with the
	// clone, the replacement carries its own scope and must not fail closed.
	liveOriginalScope.close()
	// Settled owner baseline BEFORE the replacement constructs anything
	// (proceed is still closed): the live Call owner plus any session-
	// lifetime shared toolset construction the original turn pinned.
	// The replacement's clone ALREADY assembled at interrupt time (the
	// parked replacement's CallOptions exist above), so its own
	// construction pins are registered in cloneScope and counted in this
	// baseline. The run-loop close must release EXACTLY those clone pins:
	// the final owner set is the baseline MINUS the clone's registrations —
	// more would leak the clone's pins, fewer would over-release
	// baseline owners.
	settledOwners := h.caps.ownerCount()
	clonePins := cloneScope.registeredCount()
	require.Positive(t, clonePins, "the replacement's clone scope carries its own construction pins")
	close(proceed)

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "the replacement must construct and complete successfully with the original scope closed")
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the replacement was queued and unblocked")
	}
	require.GreaterOrEqual(t, calls.Load(), int64(2),
		"the replacement must have reached the provider as a second turn")
	// R12-H2a round 2: the replacement ran as the live run loop's popped
	// call, and the run-loop close in agent_run.go's turn loop is the
	// production mechanism that closes the popped call's OWN clone scope
	// when that turn finishes (the durable path already closes its clone in
	// RunSessionAgentCall). The clone scope must therefore be CLOSED once
	// the Run that executed the replacement returned — without the run-loop
	// close nothing ever closes it (the entry defers close only the Run's
	// first call's scope). The owner-count oracle below then confirms the
	// manager settled back to the pre-replacement baseline (the live Call
	// owner plus the session-lifetime shared toolset construction captured
	// in settledOwners; the deferred release() drops the Call owner).
	require.Eventually(t, func() bool {
		return cloneScope.closed
	}, 10*time.Second, 20*time.Millisecond,
		"the replacement turn's OWN clone scope must be closed at its turn's end by the run loop")
	// R12-H2a round 2: the replacement ran as the live run loop's popped
	// call, and the run-loop close in agent_run.go's turn loop is the
	// production mechanism that closes the popped call's OWN clone scope
	// when that turn finishes (the durable path already closes its clone in
	// RunSessionAgentCall). With that close in place the replacement's
	// constructions release at its turn's end: after Run returns the
	// capability manager must be back at the Call-owner-only baseline
	// (here: the round2-queued-live Call owner plus the session-lifetime
	// shared toolset construction captured in settledOwners above; the
	// deferred release() drops the Call owner at test end).
	expectedOwners := settledOwners - clonePins
	require.Eventually(t, func() bool {
		return h.caps.ownerCount() == expectedOwners
	}, 10*time.Second, 50*time.Millisecond,
		"the replacement turn's own clone scope must be closed at its turn's end, releasing its constructions (owner set back to the Call-owner-only baseline minus the clone's own pins)")
}
