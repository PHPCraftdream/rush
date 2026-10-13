package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// R11F adapters: production implementations of every R11C/R11D/R11E seam
// over the real engine and the session store. Adapters only; nothing here is
// wired into a session or the App (flagged installation stays in M2/M3).

// batchRunnerAdapter adapts *jobbatch.Runner onto the batchRunnerHandle
// seam. Direct calls submit ONE-item task specs; the engine requires a batch
// root, so the adapter wraps the task with jobbatch.OneItem (NotifyAll, D10)
// and keeps the returned Batch for the done source. The durable reservation
// is written by the commit hook during engine Submit.
type batchRunnerAdapter struct {
	runner *jobbatch.Runner

	mu      sync.Mutex
	started map[jobbatch.ID]*jobbatch.Batch
}

func newBatchRunnerAdapter(runner *jobbatch.Runner) *batchRunnerAdapter {
	return &batchRunnerAdapter{runner: runner, started: make(map[jobbatch.ID]*jobbatch.Batch)}
}

var _ batchRunnerHandle = (*batchRunnerAdapter)(nil)

// Submit starts the caller-supplied root. The returned ID is always the
// requested root (ingress contract: runner root must equal requested root).
func (a *batchRunnerAdapter) Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	// R11F P1-6: a deferred install binds the real Runner at Start. An
	// Invoke that races ahead of Start has no Runner and fails closed.
	if a.runner == nil {
		return "", errors.New("batch runner adapter: runner not constructed yet (Start did not run)")
	}
	if req.Spec.Task == nil {
		return "", fmt.Errorf("%w: direct calls submit one task", jobbatch.ErrInvalidSpec)
	}
	batch, err := a.runner.Submit(ctx, jobbatch.SubmitRequest{
		Root: req.Root, Spec: jobbatch.OneItem(*req.Spec.Task), Ref: req.Ref,
	})
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.started[req.Root] = batch
	a.mu.Unlock()
	return req.Root, nil
}

// Stop requests cancellation of one root.
func (a *batchRunnerAdapter) Stop(ctx context.Context, id jobbatch.ID) error {
	return a.runner.Stop(ctx, id)
}

// batchFor returns the in-process Batch of a root started by THIS adapter,
// or nil (a root attached after a restart is polled durably instead).
func (a *batchRunnerAdapter) batchFor(root jobbatch.ID) *jobbatch.Batch {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started[root]
}

// batchAttachAdapter adapts session.BatchStore onto the R11B attach seam.
type batchAttachAdapter struct{ store *session.BatchStore }

var _ batchAttachStore = batchAttachAdapter{}

// ProbeLive is the read-only fence probe; reservations happen in the commit
// hook at Submit, never here.
func (a batchAttachAdapter) ProbeLive(ctx context.Context, p batchAttachProbe) (jobbatch.ID, bool, error) {
	root, ok, err := a.store.BatchProbeLive(ctx, session.BatchLiveProbe{
		Owner: p.Owner, Host: p.Host, RunnerID: p.RunnerID,
		ToolCallID: p.ToolCallID, InputHash: p.InputHash,
	})
	return jobbatch.ID(root), ok, err
}

// batchStoreDoneSource implements the R11D completion seam over the in-process
// Runner and the durable response envelope. It NEVER invents a result: every
// returned envelope is the executor's own ToolResponse, or the store's
// persisted projection of it.
type batchStoreDoneSource struct {
	store *session.BatchStore
	// batches resolves a root started in this process (nil-able lookup).
	batches func(jobbatch.ID) *jobbatch.Batch
	// poll is the durable re-read cadence for roots without a live Batch;
	// zero selects batchDonePollInterval.
	poll time.Duration
}

var _ batchDoneSource = (*batchStoreDoneSource)(nil)

// batchDonePollInterval is the fallback durable poll cadence.
const batchDonePollInterval = 250 * time.Millisecond

// AwaitDone waits for the root's REAL completion: a live Batch blocks on the
// engine (never a stop acknowledgement), then loads the persisted envelope;
// a foreign root polls the durable envelope only. ok=false means held
// termination or a caller-cancelled wait, never a fabricated settlement.
func (d *batchStoreDoneSource) AwaitDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, false, err
	}
	if b := d.batches(root); b != nil {
		if _, err := b.Wait(ctx); err != nil {
			return fantasy.ToolResponse{}, false, err
		}
		return d.LoadDone(ctx, owner, root)
	}
	tick := time.NewTicker(d.pollInterval())
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fantasy.ToolResponse{}, false, err
		}
		resp, ok, err := d.LoadDone(ctx, owner, root)
		if err != nil || ok {
			return resp, ok, err
		}
		select {
		case <-ctx.Done():
			return fantasy.ToolResponse{}, false, ctx.Err()
		case <-tick.C:
		}
	}
}

// LoadDone reads an already-persisted completion without waiting (R11F
// P1-2): the one-item completion read — the root envelope when present, else
// the settled single task child's envelope (the Settled commit hook writes
// the envelope on the settled NODE, root.1, not the root). Envelope
// details carry the executor's full ToolResponse (batchEnvelope); a settled
// row without details is projected honestly from its stored summary and
// outcome state (state failed => IsError), never fabricated.
func (d *batchStoreDoneSource) LoadDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	e, ok, err := d.store.BatchDeliveryOneItemCompletion(ctx, owner, string(root))
	if err != nil || !ok {
		return fantasy.ToolResponse{}, false, err
	}
	if len(e.Details) > 0 {
		var resp fantasy.ToolResponse
		if err := json.Unmarshal(e.Details, &resp); err != nil {
			return fantasy.ToolResponse{}, false, fmt.Errorf("batch done source: envelope %s: %w", e.Ref, err)
		}
		return resp, true, nil
	}
	resp := fantasy.NewTextResponse(e.Summary)
	resp.IsError = e.State == "failed" || e.State == "timed_out" || e.State == "cancelled"
	return resp, true, nil
}

func (d *batchStoreDoneSource) pollInterval() time.Duration {
	if d.poll > 0 {
		return d.poll
	}
	return batchDonePollInterval
}

// batchStoreElector implements the R8 election seam over the typed store
// elections. CAS semantics: exactly one winner, losers report false and
// insert no history (session.ErrBatchAnnouncementLost maps to won=false).
type batchStoreElector struct {
	store    *session.BatchStore
	messages message.Service
}

var _ batchElector = batchStoreElector{}

// ElectStarted wins-or-loses the durable started history for the root.
func (e batchStoreElector) ElectStarted(ctx context.Context, owner string, root jobbatch.ID) (bool, error) {
	params := message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: batchStartedNoticeText(root)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
	_, won, err := e.store.BatchDeliveryElectStarted(ctx, e.messages, owner, string(root), params)
	return won, err
}

// ElectInline publishes the elected completion exactly once. The history
// message carries the envelope's content; a media-only response is serialised
// so no payload is lost.
func (e batchStoreElector) ElectInline(ctx context.Context, owner string, root jobbatch.ID, envelope fantasy.ToolResponse) (bool, error) {
	_, won, err := e.store.BatchDeliveryElectInline(ctx, e.messages, owner, string(root),
		batchCompletionMessageParams(batchEnvelopeText(envelope)))
	return won, err
}

func batchStartedNoticeText(root jobbatch.ID) string {
	return fmt.Sprintf("batch root %s started; completions arrive as selected notices.", root)
}

func batchCompletionMessageParams(text string) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: text}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
}

// batchEnvelopeText renders a completion envelope for history: the content
// when present, else the serialised envelope (media/metadata preserved).
func batchEnvelopeText(envelope fantasy.ToolResponse) string {
	if envelope.Content != "" {
		return envelope.Content
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return ""
	}
	return string(raw)
}

// batchStoreNoticePull implements the drain seam over the R8 outbox pull.
// Pull consumes only ALREADY-SELECTED notices; it never waits for running
// batch work.
type batchStoreNoticePull struct {
	store    *session.BatchStore
	messages message.Service
	owner    string
}

var _ batchNoticePull = batchStoreNoticePull{}

// Pull drains the owner's pending selected notices and reports the count
// actually delivered into history.
func (p batchStoreNoticePull) Pull(ctx context.Context) (int, error) {
	pulled, err := p.store.BatchPullNotices(ctx, p.messages, p.owner, batchPulledNoticeParams)
	if err != nil {
		return 0, err
	}
	return len(pulled), nil
}

// batchPulledNoticeParams formats one selected notice for history from the
// row's own facts; nothing is re-derived from live state.
func batchPulledNoticeParams(row session.BatchNoticeRow) message.CreateMessageParams {
	origin := message.OriginWeb
	if row.OriginCLI {
		origin = message.OriginCLI
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: batchNoticeText(row)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		Origin:              origin,
	}
}

func batchNoticeText(row session.BatchNoticeRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "batch %s node %s: kind %q state %q", row.RootID, row.NodeID, row.Kind, row.State)
	if row.Cause != "" {
		fmt.Fprintf(&b, " cause %q", row.Cause)
	}
	if row.OutcomeCode != "" || row.OutcomeSummary != "" {
		fmt.Fprintf(&b, " outcome %s %s", row.OutcomeCode, row.OutcomeSummary)
	}
	fmt.Fprintf(&b, " counts pending=%d active=%d completed=%d failed=%d cancelled=%d timed_out=%d interrupted=%d skipped=%d",
		row.Pending, row.Active, row.Completed, row.Failed, row.Cancelled, row.TimedOut, row.Interrupted, row.Skipped)
	return b.String()
}

// BatchLeafRegistryStore is the pinned R1 registry resolver keyed by the
// invocation's registry identity. Registration is pinned-once: an identity
// can never be replaced, so a live call's leaf set cannot change under it.
// Lookup fails closed on an unknown identity.
type BatchLeafRegistryStore struct {
	mu         sync.RWMutex
	registries map[string]*policyLeafRegistry
}

func NewBatchLeafRegistryStore() *BatchLeafRegistryStore {
	return &BatchLeafRegistryStore{registries: make(map[string]*policyLeafRegistry)}
}

// Register pins one registry under its identity. Double registration of the
// same identity is refused, whether the registry differs or not.
func (s *BatchLeafRegistryStore) Register(registryID string, registry *policyLeafRegistry) error {
	if strings.TrimSpace(registryID) == "" {
		return errors.New("batch leaf registry: empty registry identity")
	}
	if registry == nil {
		return errors.New("batch leaf registry: nil registry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.registries[registryID]; exists {
		return fmt.Errorf("batch leaf registry: %q is already pinned", registryID)
	}
	s.registries[registryID] = registry
	return nil
}

// Resolve returns the pinned registry of the identity; an unknown identity
// fails closed (never an empty registry).
func (s *BatchLeafRegistryStore) Resolve(registryID string) (*policyLeafRegistry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	registry, ok := s.registries[registryID]
	if !ok {
		return nil, fmt.Errorf("batch leaf registry: unknown pinned registry identity %q", registryID)
	}
	return registry, nil
}

// newBatchExecutorFamilies registers the R7 executor families for the
// Runner's router: generic and MCP leaves through the pinned policy leaf,
// shell/argv with their retained handles, agent delegation via R7c.
func newBatchExecutorFamilies(agentDeps batchAgentDeps) map[string]batchLeafFamily {
	generic := newBatchGenericFamily()
	return map[string]batchLeafFamily{
		batchKindGeneric: generic,
		batchKindMCP:     generic,
		batchKindShell:   batchShellFamily{},
		batchKindArgv:    batchArgvFamily{},
		batchKindAgent:   newBatchAgentFamily(agentDeps),
	}
}

// BatchInstallDeps carries everything one flagged install needs. All fields
// are required except Executor, Limits, CommitTimeout, ControlTimeout and
// RecordRetention (zero selects the engine defaults). Construction makes no
// store or engine calls; only InstallBatchRuntime does.
type BatchInstallDeps struct {
	// Host, RunnerID and Owner are the commit-hook identity: one Runner, one
	// owner session (session.BatchCommitIdentity contract).
	Host, RunnerID, Owner string
	// Profile is the R11A closed capacity profile.
	Profile *BatchProfile
	// Registries is the pinned R1 registry resolver.
	Registries *BatchLeafRegistryStore
	// Store and Messages are the persistence and history services.
	Store    *session.BatchStore
	Messages message.Service
	// Executor optionally replaces the default R7 router; nil builds the
	// default generic/MCP/shell/argv/agent registration with the AgentDeps
	// below (zero deps keep the M1 fail-closed agent family).
	Executor jobbatch.Executor
	// AgentDeps wires the R7c agent executor family (R11F/M3 coordinator
	// seams); zero keeps the M1 fail-closed registration.
	AgentDeps BatchAgentDeps
	// Resolver resolves MCP admission capabilities (R11A); nil rejects
	// every MCP leaf as unresolved.
	Resolver BatchMCPCapabilityResolver
	// Limits, CommitTimeout, ControlTimeout and RecordRetention mirror
	// jobbatch.RunnerConfig; zero values select the engine defaults.
	Limits                        jobbatch.Limits
	CommitTimeout, ControlTimeout time.Duration
	RecordRetention               int
	// DonePoll is the done source's durable poll cadence; zero selects the
	// default.
	DonePoll time.Duration
}

// BatchInstall is one fully adapted, still-dormant installation.
type BatchInstall struct {
	mu        sync.Mutex
	runnerCfg jobbatch.RunnerConfig

	Runner       *jobbatch.Runner
	CommitHook   func(context.Context, jobbatch.Transition) error
	Runtime      *batchRuntime
	RunnerHandle *batchRunnerAdapter
	Attach       batchAttachStore
	Delivery     batchDelivery
	Done         batchDoneSource
	Elector      batchElector
	NoticePull   batchNoticePull
}

// InstallBatchRuntime builds the real Runner, commit hook, attach store,
// done source, elector, delivery seam, notice pull and ingress runtime.
// The Runner's engine loop starts here (inert until Submit); the app fault
// watcher does NOT start until its own Start is called.
func InstallBatchRuntime(ctx context.Context, deps BatchInstallDeps) (*BatchInstall, error) {
	install, err := newBatchInstall(ctx, deps, true)
	if err != nil {
		return nil, err
	}
	runner, err := install.BuildRunner(ctx)
	if err != nil {
		return nil, err
	}
	install.Runner = runner
	return install, nil
}

// batchDormantInstallOwner is the dormant DEFAULT commit-hook/delivery owner
// used when an install carries no fixed owner (R11F P1-1: the flagged app
// install no longer pins the host UUID as owner). It is NEVER written as an
// owner_session_id: the commit hook resolves every Submit's owner from the
// spec payload (BatchCommitIdentity.OwnerFor) and every delivery fences by
// the invocation's session (batchDeliveryImpl.owner).
const batchDormantInstallOwner = "batch-install:per-invocation-owner"

// batchCommitOwnerFor is the P1-1 OwnerFor resolver: the INVOKING session
// carried in the one-item spec payload (batchLeafPayload.SessionID). An
// unparseable spec keeps the dormant default (the commit hook falls back).
func batchCommitOwnerFor(spec jobbatch.Spec) string {
	if spec.Task == nil {
		return ""
	}
	var p batchLeafPayload
	if err := json.Unmarshal(spec.Task.Payload, &p); err != nil {
		return ""
	}
	return p.SessionID
}

// InstallBatchRuntimeDeferred builds everything InstallBatchRuntime builds
// EXCEPT the Runner (R11F P1-6: no engine goroutines until BatchRuntime.Start
// constructs the Runner through BatchInstall.BuildRunner via the runtime's
// RunnerFactory). The runner handle adapter is wired into the runtime
// immediately; it fails closed on Submit until BuildRunner attaches the real
// Runner.
func InstallBatchRuntimeDeferred(ctx context.Context, deps BatchInstallDeps) (*BatchInstall, error) {
	return newBatchInstall(ctx, deps, false)
}

// newBatchInstall is the shared construction core. withRunner=false keeps
// the Runner unconstructed (dormant install).
func newBatchInstall(_ context.Context, deps BatchInstallDeps, withRunner bool) (*BatchInstall, error) {
	// R11F P1-1: Owner is no longer required — the flagged install resolves
	// the per-invocation owner from the spec payload; a fixed Owner is only
	// the dormant default.
	switch {
	case deps.Host == "" || deps.RunnerID == "":
		return nil, errors.New("batch install: host and runner id are required")
	case deps.Profile == nil:
		return nil, errors.New("batch install: profile is required")
	case deps.Registries == nil:
		return nil, errors.New("batch install: pinned registry store is required")
	case deps.Store == nil:
		return nil, errors.New("batch install: batch store is required")
	case deps.Messages == nil:
		return nil, errors.New("batch install: message service is required")
	}
	owner := deps.Owner
	if owner == "" {
		owner = batchDormantInstallOwner
	}
	runnerAdapter := &batchRunnerAdapter{started: make(map[jobbatch.ID]*jobbatch.Batch)}
	done := &batchStoreDoneSource{store: deps.Store, batches: runnerAdapter.batchFor, poll: deps.DonePoll}
	elector := batchStoreElector{store: deps.Store, messages: deps.Messages}
	delivery, err := newBatchDelivery(batchDeliveryDeps{Owner: owner, Done: done, Elect: elector, Runner: runnerAdapter})
	if err != nil {
		return nil, err
	}
	env := batchLeafEnv{Leaves: deps.Registries.Resolve}
	executor := deps.Executor
	if executor == nil {
		executor = newBatchExecutor(env, newBatchExecutorFamilies(deps.AgentDeps))
	}
	// R11F P1-1: the commit hook reserves every Submit under the INVOKING
	// session (OwnerFor); the fixed Owner stays the dormant default.
	commitHook := session.NewBatchCommitHook(deps.Store, session.BatchCommitIdentity{
		Host: deps.Host, Runner: deps.RunnerID, Owner: owner, OwnerFor: batchCommitOwnerFor,
	})
	runnerCfg := jobbatch.RunnerConfig{
		CommitHook: commitHook, CommitTimeout: deps.CommitTimeout,
		Limits: deps.Limits, Executor: executor,
		ControlTimeout: deps.ControlTimeout, RecordRetention: deps.RecordRetention,
	}
	if withRunner {
		runner, err := jobbatch.NewRunner(context.Background(), runnerCfg)
		if err != nil {
			return nil, err
		}
		runnerAdapter.runner = runner
	}
	runtime, err := newBatchRuntime(batchRuntimeConfig{
		Host: deps.Host, RunnerID: deps.RunnerID, Profile: deps.Profile,
		Resolver: deps.Resolver,
		Leaves:   deps.Registries.Resolve,
		Attach:   batchAttachAdapter{store: deps.Store}, Delivery: delivery, Runner: runnerAdapter,
	})
	if err != nil {
		return nil, err
	}
	install := &BatchInstall{
		CommitHook: commitHook, Runtime: runtime,
		RunnerHandle: runnerAdapter, Attach: batchAttachAdapter{store: deps.Store},
		Delivery: delivery, Done: done, Elector: elector,
		// R11F P1-5(d): the notice-pull drain adapter has NO production
		// consumer yet — it is consumed by the web drain surface in
		// R12/R15b; wired inertly until then. Do not invent a consumer.
		NoticePull: batchStoreNoticePull{store: deps.Store, messages: deps.Messages, owner: owner},
		runnerCfg:  runnerCfg,
	}
	if withRunner {
		install.Runner = runnerAdapter.runner
	}
	return install, nil
}

// BuildRunner constructs the real Runner for a deferred install (R11F P1-6)
// and attaches it to the runner handle (making the ingress live). Idempotent
// per install: the first construction wins; later calls return the same
// Runner.
func (i *BatchInstall) BuildRunner(ctx context.Context) (*jobbatch.Runner, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Runner != nil {
		return i.Runner, nil
	}
	runner, err := jobbatch.NewRunner(ctx, i.runnerCfg)
	if err != nil {
		return nil, err
	}
	i.Runner = runner
	i.RunnerHandle.runner = runner
	return runner, nil
}
