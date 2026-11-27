// Opt-in default deadline for background bash/run_command jobs that pass no
// explicit timeout: resolved from config + per-call CallOptions, mirroring
// supervision.go's resolveSupervisionConfig precedence.
// Contract seam for the SA1 lane of the agent split (design
// docs/plans/2026-11-15-sprint0-split-agent.md §2.3/§3.1): the call
// contract lives in agentcall (a leaf that imports nothing from package
// agent); this file re-exports it under the historical agent names until
// the DM8 lane requalifies the importers.
package agent

import (
	"context"
	"io"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/wakectl"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// create выполняет SQL вне mutex; результат принимается только той же era.
func (c *coordinator) armAwait(ctx context.Context, sid string, all bool, create func() (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	era, old := c.arb.beginAwait(sid, all)
	if era == nil {
		return "", errAwaitSuperseded
	}
	c.cancelAwaitTimer(sid, old)
	id := ""
	if create != nil {
		var err error
		id, err = create()
		if err != nil {
			c.arb.abortAwait(sid, era)
			c.cancelAwaitTimer(sid, id)
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		c.arb.abortAwait(sid, era)
		c.cancelAwaitTimer(sid, id)
		return "", err
	}
	if !c.arb.installAwait(sid, era, id) {
		c.cancelAwaitTimer(sid, id)
		return "", errAwaitSuperseded
	}
	return id, nil
}

// Type aliases: the contract declarations now live in agentcall; every
// agent-qualified name keeps working unchanged (design §2.7; importers are
// requalified in DM8, not here).
type (
	Coordinator             = agentcall.Coordinator
	SessionAgent            = agentcall.SessionAgent
	SessionAgentCall        = agentcall.SessionAgentCall
	Model                   = agentcall.Model
	ModelOverride           = agentcall.ModelOverride
	AwaitingAnswerError     = agentcall.AwaitingAnswerError
	CallOptions             = agentcall.CallOptions
	RequestedModelRole      = agentcall.RequestedModelRole
	ProviderType            = agentcall.ProviderType
	Role                    = agentcall.Role
	Credential              = agentcall.Credential
	CredentialModel         = agentcall.CredentialModel
	CredentialSet           = agentcall.CredentialSet
	ModelChoice             = agentcall.ModelChoice
	SummarizeSnapshot       = agentcall.SummarizeSnapshot
	ErrCallAlreadyAttempted = agentcall.ErrCallAlreadyAttempted
	// aliases under the historical unexported names for the staying
	// package-agent code
	readyGate                 = agentcall.ReadyGate
	buildResourceScope        = agentcall.BuildResourceScope
	codexRecoveryState        = agentcall.CodexRecoveryState
	batchChildQuestionState   = agentcall.BatchChildQuestionState
	batchAgentQuestionBinding = agentcall.BatchAgentQuestionBinding
)

const (
	DefaultSessionName    = agentcall.DefaultSessionName
	DefaultCancelAllGrace = agentcall.DefaultCancelAllGrace

	awaitingAnswerStoppedTitle = agentcall.AwaitingAnswerStoppedTitle

	RoleSmart    = agentcall.RoleSmart
	RoleFast     = agentcall.RoleFast
	RoleWorker   = agentcall.RoleWorker
	RoleReviewer = agentcall.RoleReviewer

	ProviderTypeOpenAI       = agentcall.ProviderTypeOpenAI
	ProviderTypeOpenAICompat = agentcall.ProviderTypeOpenAICompat
	ProviderTypeOpenRouter   = agentcall.ProviderTypeOpenRouter
	ProviderTypeAnthropic    = agentcall.ProviderTypeAnthropic
	ProviderTypeGoogle       = agentcall.ProviderTypeGoogle
	ProviderTypeGoogleVertex = agentcall.ProviderTypeGoogleVertex
	ProviderTypeAzure        = agentcall.ProviderTypeAzure
	ProviderTypeBedrock      = agentcall.ProviderTypeBedrock
	ProviderTypeVercel       = agentcall.ProviderTypeVercel
)

var (
	ErrRequestCancelled      = agentcall.ErrRequestCancelled
	ErrSessionBusy           = agentcall.ErrSessionBusy
	ErrEmptyPrompt           = agentcall.ErrEmptyPrompt
	ErrSessionMissing        = agentcall.ErrSessionMissing
	ErrAgentShuttingDown     = agentcall.ErrAgentShuttingDown
	ErrTurnStalled           = agentcall.ErrTurnStalled
	ErrAgentRoleInvalid      = agentcall.ErrAgentRoleInvalid
	ErrAgentRoleUnconfigured = agentcall.ErrAgentRoleUnconfigured
	ErrAgentRoleMismatch     = agentcall.ErrAgentRoleMismatch
	// unexported names the staying code compares against; same error values
	// as the agentcall sentinels, so errors.Is behavior is unchanged
	errAwaitingAnswer = agentcall.ErrAwaitingAnswer
	userAgent         = agentcall.UserAgent

	ErrCallOptionsSpecVersionUnsupported = agentcall.ErrCallOptionsSpecVersionUnsupported
)

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WebModelPolicy(ctx context.Context) bool { return agentcall.WebModelPolicy(ctx) }

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithCallOptions(ctx context.Context, o *CallOptions) context.Context {
	return agentcall.WithCallOptions(ctx, o)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func callOptionsFrom(ctx context.Context) *CallOptions { return agentcall.CallOptionsFrom(ctx) }

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func EnsureCallBuildScope(opts *CallOptions) *buildResourceScope {
	return agentcall.EnsureCallBuildScope(opts)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func newBuildResourceScope() *buildResourceScope { return agentcall.NewBuildResourceScope() }

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithRateLimitNotifySink(ctx context.Context, w io.Writer) context.Context {
	return agentcall.WithRateLimitNotifySink(ctx, w)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func RateLimitNotifySinkFrom(ctx context.Context) io.Writer {
	return agentcall.RateLimitNotifySinkFrom(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithSessionModelPersistence(ctx context.Context, smart, fast *session.ModelSlotUpdate) context.Context {
	return agentcall.WithSessionModelPersistence(ctx, smart, fast)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithRoleReasoningEffort(ctx context.Context, effort string) context.Context {
	return agentcall.WithRoleReasoningEffort(ctx, effort)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithModelOverrides(ctx context.Context, smart, fast *ModelOverride) context.Context {
	return agentcall.WithModelOverrides(ctx, smart, fast)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithFolderScopeSpec(ctx context.Context, spec *permission.FolderScopeSpec) context.Context {
	return agentcall.WithFolderScopeSpec(ctx, spec)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithRunAllowlist(ctx context.Context, a *permission.RunAllowlist) context.Context {
	return agentcall.WithRunAllowlist(ctx, a)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func WithRunAllowlistSpec(ctx context.Context, spec *permission.RunAllowlistSpec) context.Context {
	return agentcall.WithRunAllowlistSpec(ctx, spec)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func withReadyGate(ctx context.Context, gate *readyGate) context.Context {
	return agentcall.WithReadyGate(ctx, gate)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func readyGateFor(ctx context.Context, fallback *readyGate) *readyGate {
	return agentcall.ReadyGateFor(ctx, fallback)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func AwaitingAnswerGuidance(question string, options []string, sessionID string, role string) string {
	return agentcall.AwaitingAnswerGuidance(question, options, sessionID, role)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func awaitingAnswerGuidance(question string, options []string, sessionID string, role string, windowsShell bool) string {
	return agentcall.AwaitingAnswerGuidanceShell(question, options, sessionID, role, windowsShell)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func quoteResumeSessionID(sessionID string, windowsShell bool) (string, bool) {
	return agentcall.QuoteResumeSessionID(sessionID, windowsShell)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func ToSessionAgentCallData(call SessionAgentCall) session.SessionAgentCallData {
	return agentcall.ToSessionAgentCallData(call)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func FromSessionAgentCallData(callData session.SessionAgentCallData) (SessionAgentCall, error) {
	return agentcall.FromSessionAgentCallData(callData)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func BuildSessionExecutionRequirement(bindingID, owner string, epoch int64, folderScopeSpec *permission.FolderScopeSpec, runAllowlistSpec permission.RunAllowlistSpec, opts *CallOptions) *session.SessionExecutionRequirement {
	return agentcall.BuildSessionExecutionRequirement(bindingID, owner, epoch, folderScopeSpec, runAllowlistSpec, opts)
}

// ownDeferredCallScope copies trusted per-call options for a deferred call
// and gives that copy a fresh build-resource scope. The binding handle stays
// borrowed and immutable; the caller's options are never modified.
func ownDeferredCallScope(ctx context.Context) (context.Context, *buildResourceScope) {
	opts := callOptionsFrom(ctx)
	if opts == nil {
		return ctx, nil
	}
	owned := *opts
	owned.BuildScope = newBuildResourceScope()
	return WithCallOptions(ctx, &owned), owned.BuildScope
}

// callBuildScopeFrom returns the trusted call state's build-resource scope
// carried by ctx (nil for legacy contexts without per-call options). Tool
// construction registers its Construction releases in this scope.
func callBuildScopeFrom(ctx context.Context) *buildResourceScope {
	if opts := callOptionsFrom(ctx); opts != nil {
		return EnsureCallBuildScope(opts)
	}
	return nil
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func folderScopeSpecFrom(ctx context.Context) *permission.FolderScopeSpec {
	return agentcall.FolderScopeSpecFrom(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func runAllowlistFrom(ctx context.Context) *permission.RunAllowlist {
	return agentcall.RunAllowlistFrom(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func runAllowlistSpecFrom(ctx context.Context) *permission.RunAllowlistSpec {
	return agentcall.RunAllowlistSpecFrom(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func modelOverridesFrom(ctx context.Context) (smart, fast *ModelOverride) {
	return agentcall.ModelOverridesFrom(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func sessionModelPersistenceFrom(ctx context.Context) (smart, fast *session.ModelSlotUpdate) {
	return agentcall.SessionModelPersistenceFrom(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func roleReasoningEffortFrom(ctx context.Context) string {
	return agentcall.RoleReasoningEffortFrom(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func inheritWebPolicyCallOptions(ctx context.Context) *CallOptions {
	return agentcall.InheritWebPolicyCallOptions(ctx)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func awaitingAnswerStoppedFinishText(err error, role string) (msg, details string) {
	return agentcall.AwaitingAnswerStoppedFinishText(err, role)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func fromSessionFolderScopeSpec(spec *session.FolderScopeSpec) *permission.FolderScopeSpec {
	return agentcall.FromSessionFolderScopeSpec(spec)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func toSessionFolderScopeSpec(spec *permission.FolderScopeSpec) *session.FolderScopeSpec {
	return agentcall.ToSessionFolderScopeSpec(spec)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func fromSessionRunAllowlistSpec(spec *session.RunAllowlistSpec) *permission.RunAllowlistSpec {
	return agentcall.FromSessionRunAllowlistSpec(spec)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func toSessionRunAllowlistSpec(spec *permission.RunAllowlistSpec) *session.RunAllowlistSpec {
	return agentcall.ToSessionRunAllowlistSpec(spec)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func toSessionModelCfg(cfg config.SelectedModel) session.ModelCfg {
	return agentcall.ToSessionModelCfg(cfg)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func fromSessionModelCfg(cfg session.ModelCfg) config.SelectedModel {
	return agentcall.FromSessionModelCfg(cfg)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func toSessionModelSlotUpdate(update *session.ModelSlotUpdate) *session.ModelSlotUpdate {
	return agentcall.ToSessionModelSlotUpdate(update)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func toSessionCallOptionsSpec(o *CallOptions) *session.CallOptionsSpec {
	return agentcall.ToSessionCallOptionsSpec(o)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func fromSessionCallOptionsSpec(spec *session.CallOptionsSpec) (*CallOptions, error) {
	return agentcall.FromSessionCallOptionsSpec(spec)
}

// aliases under the historical unexported names for the staying package-agent code
type callOptionsContextKey = agentcall.CallOptionsContextKey

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func subAgentQuestionText(childSessionID string, ae *AwaitingAnswerError) string {
	return agentcall.SubAgentQuestionText(childSessionID, ae)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func subAgentQuestionWithPreamble(preamble, frame string) string {
	return agentcall.SubAgentQuestionWithPreamble(preamble, frame)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func subAgentQuestionFromFinish(childSessionID string, fp *message.Finish) (string, bool) {
	return agentcall.SubAgentQuestionFromFinish(childSessionID, fp)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func subAgentQuestionBlockFromFinish(fp *message.Finish) (string, bool) {
	return agentcall.SubAgentQuestionBlockFromFinish(fp)
}

// thin delegate to agentcall (name kept for staying/external callers; requalified in DM8).
func diskRequirementForCall(call SessionAgentCall) *session.SessionExecutionRequirement {
	return agentcall.DiskRequirementForCall(call)
}

func WithAdmissionSetup(ctx context.Context, fn func(context.Context) error) context.Context {
	return turnstep.WithAdmissionSetup(ctx, fn)
}

func admissionSetupFrom(ctx context.Context) func(context.Context) error {
	return turnstep.AdmissionSetupFrom(ctx)
}

// ResolveRoleContract validates and snapshots an exact configured role at admission.
func ResolveRoleContract(cfg *config.Config, name string, inherited *session.RoleContract) (*session.RoleContract, error) {
	return modelkit.ResolveRoleContract(cfg, name, inherited)
}

// Compatibility ports for batch callers that remain in the root package.
func inheritedRoleCeilings(parent *session.RoleContract) ([][]string, error) {
	return modelkit.InheritedRoleCeilings(parent)
}

func cloneRoleContractForCall(contract *session.RoleContract) *session.RoleContract {
	return agentcall.CloneRoleContractForCall(contract)
}

func roleContractAllowsTool(contract *session.RoleContract, name string) bool {
	return modelkit.RoleContractAllowsTool(contract, name)
}

// WithSelectedRoleContract transports a batch-admitted immutable role choice.
func WithSelectedRoleContract(ctx context.Context, contract *session.RoleContract) context.Context {
	return modelkit.WithSelectedRoleContract(ctx, contract)
}

// WithCallRoleContract transports a detached immutable role snapshot through a call context.
func WithCallRoleContract(ctx context.Context, contract *session.RoleContract) context.Context {
	return modelkit.WithCallRoleContract(ctx, contract)
}

// WithRoleContractCopy returns options with a detached contract and the original execution policy.
func WithRoleContractCopy(opts *CallOptions, contract *session.RoleContract) *CallOptions {
	return modelkit.WithRoleContractCopy(opts, contract)
}

// resolveBackgroundJobDefaultTimeout returns the effective default
// terminate_and_wake deadline for a background bash/run_command call: the
// run's CallOptions override (`rush run --job-timeout`) when positive,
// otherwise config's background_job_default_timeout_seconds, otherwise zero
// (off). The 5-second timeoutSecondsFloor does NOT apply here: this is an
// operator-configured value, not a model-supplied one.
func (c *coordinator) resolveBackgroundJobDefaultTimeout(ctx context.Context) time.Duration {
	if callOpts := callOptionsFrom(ctx); callOpts != nil && callOpts.BackgroundJobDefaultTimeout > 0 {
		return callOpts.BackgroundJobDefaultTimeout
	}
	if c != nil && c.cfg != nil {
		if opts := c.cfg.Config().Options; opts != nil && opts.BackgroundJobDefaultTimeoutSeconds > 0 {
			return time.Duration(opts.BackgroundJobDefaultTimeoutSeconds) * time.Second
		}
	}
	return 0
}

type SessionWakeDispatcher interface {
	WakeSession(context.Context, string, bool) error
}

func DispatchSessionWake(ctx context.Context, c Coordinator, owner string) error {
	return wakectl.DispatchSessionWake(ctx, c, owner)
}

func (c *coordinator) WakeSession(ctx context.Context, owner string, fact bool) error {
	return c.wakeSession(ctx, owner, fact)
}
