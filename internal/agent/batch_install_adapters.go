package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
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
	outputCache *batchOutputCache
	runner      *jobbatch.Runner
	credentials *batchCredentialEscrow
	state       batchadapter.RunnerState

	// Compatibility views for SubmitCLIRef's paired update. The state owns
	// both maps and their lock; these are not separate storage.
	mu      *sync.Mutex
	started map[jobbatch.ID]*jobbatch.Batch
	owners  map[jobbatch.ID]string
}

func newBatchRunnerAdapter(runner *jobbatch.Runner) *batchRunnerAdapter {
	return bindBatchRunnerAdapter(runner, nil, newBatchCredentialEscrow())
}

func bindBatchRunnerAdapter(runner *jobbatch.Runner, cache *batchOutputCache, credentials *batchCredentialEscrow) *batchRunnerAdapter {
	a := &batchRunnerAdapter{runner: runner, outputCache: cache, credentials: credentials, state: batchadapter.NewRunnerState()}
	a.mu, a.started, a.owners = a.state.CLIState()
	return a
}

var _ batchRunnerHandle = (*batchRunnerAdapter)(nil)

func (a *batchRunnerAdapter) OutputCache() *batchOutputCache { return a.outputCache }

func (a *batchRunnerAdapter) engineAdapter() *batchadapter.Runner {
	return &batchadapter.Runner{
		Engine:             func() *jobbatch.Runner { return a.runner },
		Stamp:              stampBatchCallCredentials,
		Register:           a.registerCredentials,
		ReleaseCredentials: a.credentials.releaseRoot,
		State:              &a.state,
	}
}

func (a *batchRunnerAdapter) registerCredentials(ctx context.Context, root string, specs []jobbatch.Spec) (func(), error) {
	var registrations []batchadapter.CredentialRegistration
	for _, spec := range specs {
		entries, err := batchadapter.CollectCredentialRegistrations(root, spec, batchKindAgent, callCredentialsFrom(ctx), decodeBatchCredentialRegistration)
		if err != nil {
			return nil, err
		}
		registrations = append(registrations, entries...)
	}
	if a.credentials != nil {
		return a.credentials.RegisterBatch(registrations)
	}
	if len(registrations) > 0 {
		return nil, errBatchCredentialEscrowMissing
	}
	return func() {}, nil
}

func (a *batchRunnerAdapter) Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	return a.engineAdapter().Submit(ctx, req)
}

func (a *batchRunnerAdapter) SubmitBound(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error) {
	return a.engineAdapter().SubmitBound(ctx, req)
}

func (a *batchRunnerAdapter) Stop(ctx context.Context, id jobbatch.ID) error {
	return a.engineAdapter().Stop(ctx, id)
}

func (a *batchRunnerAdapter) Release(ctx context.Context, id jobbatch.ID) error {
	return a.engineAdapter().Release(ctx, id)
}

func (a *batchRunnerAdapter) Add(ctx context.Context, req jobbatch.AddRequest) ([]jobbatch.ID, error) {
	return a.engineAdapter().Add(ctx, req)
}

func (a *batchRunnerAdapter) Snapshot(ctx context.Context, id jobbatch.ID) (jobbatch.BatchView, error) {
	return a.engineAdapter().Snapshot(ctx, id)
}

func (a *batchRunnerAdapter) View(ctx context.Context, id jobbatch.ID) (jobbatch.NodeView, error) {
	return a.engineAdapter().View(ctx, id)
}

func (a *batchRunnerAdapter) Output(ctx context.Context, id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, error) {
	return a.engineAdapter().Output(ctx, id, cursor)
}

func (a *batchRunnerAdapter) credentialRegistrations(ctx context.Context, root string, spec jobbatch.Spec) ([]batchCredentialRegistration, error) {
	entries, err := batchadapter.CollectCredentialRegistrations(root, spec, batchKindAgent, callCredentialsFrom(ctx), decodeBatchCredentialRegistration)
	if len(entries) == 0 {
		return nil, err
	}
	result := make([]batchCredentialRegistration, len(entries))
	for i, entry := range entries {
		k := entry.Key
		result[i] = batchCredentialRegistration{key: batchCredentialKey{registryID: k.RegistryID, sessionID: k.SessionID, messageID: k.MessageID, callID: k.CallID, tool: k.Tool}, root: entry.Root, creds: entry.Credentials}
	}
	return result, err
}

// batchFor returns the in-process Batch of a root started by THIS adapter,
// or nil (a root attached after a restart is polled durably instead).
func (a *batchRunnerAdapter) batchFor(root jobbatch.ID) *jobbatch.Batch {
	return a.state.BatchFor(root)
}

func (a *batchRunnerAdapter) ownerFor(root jobbatch.ID) string {
	return a.state.OwnerFor(root)
}

// Compatibility for cutover store access and existing installer tests.
type batchAttachAdapter struct{ store *session.BatchStore }

func (a batchAttachAdapter) ProbeLive(ctx context.Context, p batchAttachProbe) (jobbatch.ID, bool, error) {
	return (batchadapter.StoreAttach{Store: a.store}).ProbeLive(ctx, p)
}

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
	// owner session (coresession.BatchCommitIdentity contract).
	Host, RunnerID, Owner string
	// Profile is the R11A closed capacity profile.
	Profile *BatchProfile
	// Registries is the pinned R1 registry resolver (identity index only:
	// retention is the capability manager's owner set, never refs).
	Registries *BatchLeafRegistryStore
	// Store and Messages are the persistence and history services.
	Store    *session.BatchStore
	Messages message.Service
	// Executor optionally replaces the default R7 router; nil builds the
	// default generic/MCP/shell/argv/agent registration with the AgentDeps
	// below (zero deps keep the M1 fail-closed agent family).
	Executor jobbatch.Executor
	// DecorateExecution is an optional per-install wrapper applied to each
	// successfully started default-family execution before capability leases.
	// Nil leaves the production execution path unchanged.
	DecorateExecution func(jobbatch.Execution) jobbatch.Execution
	// AgentDeps wires the R7c agent executor family (R11F/M3 coordinator
	// seams); zero keeps the M1 fail-closed registration.
	AgentDeps BatchAgentDeps
	// Resolver resolves MCP admission capabilities (R11A); nil rejects
	// every MCP leaf as unresolved.
	Resolver installwire.MCPCapabilityResolver
	// Limits, CommitTimeout, ControlTimeout and RecordRetention mirror
	// jobbatch.RunnerConfig; zero values select the engine defaults.
	Limits                        jobbatch.Limits
	CommitTimeout, ControlTimeout time.Duration
	RecordRetention               int
	// DonePoll is the done source's durable poll cadence; zero selects the
	// default.
	DonePoll time.Duration
	// CompletionWake, when non-nil, receives the touched root after every
	// successful commit that selected completion notices (the ordinary
	// completion wake bridge; the App wires the bounded timeout-wake
	// dispatcher). Nil keeps the plain durable commit hook.
	CompletionWake func(context.Context, string)
	QuestionWake   func(context.Context, string)
	// ArmSupervision, when non-nil, arms the invoking session's supervision
	// timer when a batch delivery answers with the started response (psv
	// legacy-parity restoration). The app factory wires
	// PsvBatchSupervisionArmingLate over the late-bound coordinator. Nil
	// (older wiring, tests) arms nothing.
	ArmSupervision func(context.Context, string)
	// FinalizationAuthorityFor is the process-wide owner-to-authority
	// lookup the composition root installs when the finalization flag is
	// on; it lets the commit hook admit batch commits for the bound owner
	// without the submitting call's context. Nil keeps the exact legacy
	// behavior.
	FinalizationAuthorityFor func(string) *coresession.AgentFinalizationAuthority
	Sessions                 coresession.Service
	DiskBindings             *DiskProviderRegistry
}

// pwCompletionWakeHook wraps a durable batch commit hook with the ordinary
// selected-completion wake. After a SUCCESSFUL commit whose transition
// selected completion notices, it calls wake with the touched root; the
// receiving dispatcher resolves the owner from the durable row. Host-closing
// mirrors the wrapped hook's sticky closing flag: a HostClosing commit (and
// every later interrupted settlement, whose notices persist wake=0) never
// wakes, so shutdown and recovery launches nothing. The wake is fired
// AFTER the commit returned nil and is nonblocking; a failed commit never
// wakes.
func pwCompletionWakeHook(next func(context.Context, jobbatch.Transition) error, wake func(context.Context, string)) func(context.Context, jobbatch.Transition) error {
	closing := false
	return func(ctx context.Context, t jobbatch.Transition) error {
		err := next(ctx, t)
		if err == nil && t.Event.Type == jobbatch.EventHostClosing {
			closing = true
		}
		if err != nil || closing || wake == nil || len(t.Notices) == 0 {
			return err
		}
		// The touched roots come from the SELECTED NOTICES: Event.Root is
		// zero for a settled/control event (only submit and release carry
		// it), so it cannot identify the root of an ordinary completion.
		var woken map[jobbatch.ID]bool
		for _, n := range t.Notices {
			if n.Root == "" || woken[n.Root] {
				continue
			}
			if woken == nil {
				woken = make(map[jobbatch.ID]bool, len(t.Notices))
			}
			woken[n.Root] = true
			wake(ctx, string(n.Root))
		}
		return nil
	}
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
	Sessions     coresession.Service
	CLIPump      *session.BatchControlPump
	DiskBindings *DiskProviderRegistry
	// RootLeases is the per-root capability identity store the model control
	// shares with the runtime; exported so app wiring can pass it through
	// (NewBatchModelControl) without a runtime accessor.
	RootLeases  *BatchRootLeaseStore
	OutputCache *batchOutputCache
	cliAccepted map[string]int64
}

// BindFinalizationResultGate installs the terminal gate that withholds batch
// result reads until the owner's finalization record seals. Nil gate keeps
// the ungated legacy reads; only the durable done source is affected.
func (i *BatchInstall) BindFinalizationResultGate(gate coresession.FinalizationTerminalGate) {
	if i == nil {
		return
	}
	if done, ok := i.Done.(*batchadapter.StoreDoneSource); ok && done != nil {
		done.BindFinalizationResultGate(gate)
	}
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
// carried in the one-item spec payload (batchLeafPayload.SessionID). The
// runner adapter submits jobbatch.OneItem(*req.Spec.Task) — a BATCH-root
// spec (OneItem: Mode parallel, NotifyAll, Items=[{Task}]) whose task rides
// in spec.Batch.Items[0].Task — so the resolver walks the batch items too.
// An unparseable spec (or a spec with neither shape) keeps the dormant
// default: the commit hook falls back and the fixed/dormant Owner applies.
func batchCommitOwnerFor(spec jobbatch.Spec) string {
	if spec.Task != nil {
		var p batchLeafPayload
		if err := json.Unmarshal(spec.Task.Payload, &p); err != nil {
			return ""
		}
		return p.SessionID
	}
	if spec.Batch == nil {
		return ""
	}
	for _, child := range spec.Batch.Items {
		if owner := batchCommitOwnerFor(child); owner != "" {
			return owner
		}
	}
	return ""
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
	if owner == "" || owner == batchDormantInstallOwner {
		owner = batchDormantInstallOwner
	}
	cache := newBatchOutputCache()
	runnerAdapter := bindBatchRunnerAdapter(nil, cache, deps.Registries.credentialEscrow())
	done := batchadapter.NewStoreDoneSource(deps.Store, runnerAdapter.state.BatchFor, deps.DonePoll)
	elector := batchadapter.NewStoreElector(deps.Store, deps.Messages)
	delivery, err := newBatchDelivery(batchDeliveryDeps{
		Owner: owner, Done: done, Elect: elector, Runner: runnerAdapter, ArmSupervision: deps.ArmSupervision,
		ToolResultReceipt: func(ctx context.Context, owner string, root jobbatch.ID, callID string, sink *toolResultReceiptSink) error {
			if sink == nil {
				return nil
			}
			snapshot, err := deps.Store.Read(ctx, owner, string(root))
			if err != nil {
				return err
			}
			n := snapshot.Root
			if !n.ToolCallID.Valid || n.ToolCallID.String == "" || n.ToolCallID.String != callID || n.OwnerSessionID != owner || n.RootID != string(root) || n.NodeID != n.RootID || n.HostID == "" || n.RunnerID == "" {
				return errors.New("batch tool result: root identity mismatch")
			}
			return sink.Add(batchmeta.NewReceipt(
				session.BatchToolResultIdentity{Owner: owner, RootID: n.RootID, NodeID: n.NodeID, HostID: n.HostID, RunnerID: n.RunnerID, ToolCallID: n.ToolCallID.String},
				func(ctx context.Context, params message.CreateMessageParams) (message.Message, bool, error) {
					return deps.Store.BatchConsumeToolResult(ctx, deps.Messages, owner,
						session.BatchToolResultIdentity{Owner: owner, RootID: n.RootID, NodeID: n.NodeID, HostID: n.HostID, RunnerID: n.RunnerID, ToolCallID: n.ToolCallID.String},
						params)
				},
			))
		},
	})
	if err != nil {
		return nil, err
	}
	if deps.DiskBindings == nil {
		return nil, errors.New("batch install: app-owned disk provider registry is required")
	}
	// The capability manager drops identity-index entries when a construction
	// record dies — the index is a lookup, never an ownership counter.
	deps.DiskBindings.setRegistryDrop(deps.Registries.Delete)
	rootLeases := newBatchRootLeaseStore(deps.DiskBindings)
	// R12-H2 3c: the runtime is constructed BEFORE the executor env so the
	// env can carry a lazy ingress accessor — the App binds the ingress only
	// AFTER install (BindRuntimeIngress), so an eager value copy would stay
	// nil forever and the agent driver would fail closed for every
	// provider-bound agent incarnation.
	runtime, err := newBatchRuntime(batchRuntimeConfig{
		Host: deps.Host, RunnerID: deps.RunnerID, Profile: deps.Profile,
		Resolver: deps.Resolver, Leaves: deps.Registries.Resolve,
		RegistryCount: deps.Registries.Count,
		DiskBindings:  deps.DiskBindings,
		RootLeases:    rootLeases,
		Attach:        batchAttachAdapter{store: deps.Store}, Delivery: delivery, Runner: runnerAdapter,
	})
	if err != nil {
		return nil, err
	}
	env := batchLeafEnv{OutputCache: cache, Leaves: deps.Registries.Resolve, DiskBindings: deps.DiskBindings, Ingress: runtime.ingress}
	if deps.AgentDeps.Runs != nil {
		deps.AgentDeps.QuestionPublisher = batchCoordinatorQuestionPublisher{store: deps.Store, wake: deps.QuestionWake}
		// The store is the durable half of the question lifecycle: accepted
		// answers and cancelled asks close the notices they advertised.
		deps.AgentDeps.QuestionResolver = batchStoreQuestionResolver{store: deps.Store}
		deps.AgentDeps.Host = deps.Host
		deps.AgentDeps.RunnerID = deps.RunnerID
		deps.AgentDeps.Bind = batchStoreChildBind(deps.Store, deps.Host, deps.RunnerID)
	}
	executor := deps.Executor
	if executor == nil {
		built := newBatchExecutor(env, newBatchExecutorFamilies(deps.AgentDeps))
		built.decorateExecution = deps.DecorateExecution
		executor = built
	}
	// R11F P1-1: the commit hook reserves every Submit under the INVOKING
	// session (OwnerFor); the fixed Owner stays the dormant default.
	commitIdentity := coresession.BatchCommitIdentity{
		Host: deps.Host, Runner: deps.RunnerID, Owner: owner, OwnerFor: batchCommitOwnerFor,
		AuthorityFor: deps.FinalizationAuthorityFor,
	}
	if authorityService, ok := deps.Sessions.(coresession.AgentFinalizationService); ok {
		commitIdentity, err = authorityService.WithAgentFinalizationAuthorities(commitIdentity)
		if err != nil {
			return nil, fmt.Errorf("batch install: finalization authority: %w", err)
		}
	}
	commitHook := session.NewBatchCommitHook(deps.Store, commitIdentity)
	// Ordinary-completion wake bridge: after a SUCCESSFUL durable commit
	// that selected completion notices, notify the owning session so the
	// coordinator can run the reaction through its existing gates. The
	// wrapper only ENQUEUES the touched root (nonblocking, bounded on the
	// receiver side); it never runs a provider turn on the Runner commit
	// stack and never adds a second delivery authority. Nil (flag-OFF
	// installs, dormant tests) keeps the hook byte-identical.
	if deps.CompletionWake != nil {
		commitHook = pwCompletionWakeHook(commitHook, deps.CompletionWake)
	}
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
	install := &BatchInstall{
		CommitHook: commitHook, Runtime: runtime,
		RunnerHandle: runnerAdapter, Attach: batchAttachAdapter{store: deps.Store},
		Delivery: delivery, Done: done, Elector: elector,
		cliAccepted: make(map[string]int64),
		// R11F P1-5(d): the notice-pull drain adapter has NO production
		// consumer yet — it is consumed by the web drain surface in
		// R12/R15b; wired inertly until then. Do not invent a consumer.
		NoticePull:   batchadapter.NewStoreNoticePull(deps.Store, deps.Messages, owner),
		Sessions:     deps.Sessions,
		DiskBindings: deps.DiskBindings,
		RootLeases:   rootLeases,
		OutputCache:  cache,
		runnerCfg:    runnerCfg,
	}
	if withRunner {
		install.Runner = runnerAdapter.runner
	}
	install.CLIPump = session.NewBatchControlPump(deps.Store, deps.Host, install.commitCLIControl)
	return install, nil
}

// DrainDiskBindings closes residual construction scopes on the GRACEFUL
// shutdown path (after calls joined and disposal drained): construction
// records whose only remaining owners are their own Construction pins are
// released, so unused toolsets never outlive the calls that built them.
// R12-H2b §2.3: residual Continuation owners are dropped alongside —
// durable rows are picked up by the next process against ITS OWN store,
// so an in-process Continuation owner must not pin a binding forever.
// Forced shutdown never calls this — the manager, its remaining owners and
// the writer are retained until process exit; owners are never
// bulk-decremented.
func (i *BatchInstall) DrainDiskBindings() {
	if i == nil || i.DiskBindings == nil {
		return
	}
	i.DiskBindings.releaseResidualConstructions()
	i.DiskBindings.releaseResidualContinuations()
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

// batchStoreChildBind adapts the store's durable child-link write onto the
// agent family's bind seam; the install's host and runner identity close
// over the returned closure.
func batchStoreChildBind(store *session.BatchStore, host, runner string) func(context.Context, string, string, string, uint64, string) error {
	return func(ctx context.Context, owner, root, node string, token uint64, child string) error {
		return store.BatchBindChildSession(ctx, owner, host, runner, root, node, token, child)
	}
}
