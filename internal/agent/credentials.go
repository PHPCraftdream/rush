// Per-call credential isolation for the embeddable SDK
// (sdk.Client.RunWithCredentials): the wire types an embedder uses to
// hand Rush a tenant's own provider credentials for exactly one call,
// and the coordinator path that builds ad-hoc provider clients from
// them instead of reading the shared config/session state.
//
// Threading model in one paragraph: RunWithCredentials resolves the
// turn's smart/fast Models from the CredentialSet — never touching
// c.currentAgent's shared model values or the model cache — and pins
// them onto the SessionAgentCall through the same resolvedOverrides.pin
// path RunWithOverrides uses. The CredentialSet itself rides the call's
// context so sub-agent spawns inside the turn (runSubAgent) build their
// own ad-hoc model from the SAME tenant credentials. Every ad-hoc
// provider client is built fresh per call and never cached.

package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// callCredentialsContextKey carries the parent call's CredentialSet
// through the turn so sub-agent spawns (runSubAgent) resolve their
// model from the same tenant credentials instead of the shared config.
// Context values survive the turn's WithValue chain into every tool
// Execute closure.
type callCredentialsContextKey struct{}

func withCallCredentials(ctx context.Context, creds *CredentialSet) context.Context {
	return context.WithValue(ctx, callCredentialsContextKey{}, creds)
}

func callCredentialsFrom(ctx context.Context) *CredentialSet {
	if ctx == nil {
		return nil
	}
	creds, _ := ctx.Value(callCredentialsContextKey{}).(*CredentialSet)
	return creds
}

// credentialRunner is the shape internal/app type-asserts on a
// Coordinator to expose RunWithCredentials without widening the
// Coordinator interface (whose many existing test fakes would all have
// to grow a stub otherwise). Consuming-package interface pattern: the
// assertion lives here as a compile-time guarantee, the actual
// type-assertion happens in internal/app.
type credentialRunner interface {
	RunWithCredentials(ctx context.Context, sessionID, prompt string, creds *CredentialSet, attachments ...message.Attachment) (*fantasy.AgentResult, error)
}

var _ credentialRunner = (*coordinator)(nil)

// RunWithCredentials is like Run but resolves this call's smart/fast
// models — and any sub-agent spawn's worker model inside the turn —
// from the given CredentialSet instead of config/session state.
// Uncovered smart/fast roles are a hard error before any provider
// traffic unless creds.AllowConfiguredRoleFallback is set (see
// CredentialSet).
// Built for the embeddable SDK's concurrent multi-tenant story:
// several RunWithCredentials calls may be in flight on ONE coordinator,
// each fully isolated by its own credentials. Nothing here touches the
// coordinator's shared model values, the model cache, or any other
// call's state, and every ad-hoc provider client is built fresh per
// call (never cached).
func (c *coordinator) RunWithCredentials(ctx context.Context, sessionID, prompt string, creds *CredentialSet, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// R12-H2a round 2: resolution builds this call's toolset into the
	// trusted call state's build-resource scope before runInternal runs, so
	// every return from here — resolution failure included — must release
	// it. Close is idempotent; runInternal's own defer double-closes
	// harmlessly.
	if callOpts := callOptionsFrom(ctx); callOpts != nil {
		defer callOpts.BuildScope.Close()
	}
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}
	if creds == nil {
		return nil, fmt.Errorf("RunWithCredentials: credentials are required")
	}
	// Retain an immutable deep copy for the whole in-process call. The
	// context and SessionAgentCall must carry the same snapshot so an
	// interrupt cannot rebuild this call through operator configuration.
	callCreds := creds.Clone()
	if err := callCreds.Validate(); err != nil {
		return nil, fmt.Errorf("RunWithCredentials: %w", err)
	}

	pinned, err := c.resolveCredentialsModels(ctx, sessionID, &callCreds)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve models from per-call credentials: %w", err)
	}

	setup := admissionSetupFrom(ctx)
	ctx = WithAdmissionSetup(ctx, func(admitCtx context.Context) error {
		if setup != nil {
			if err := setup(admitCtx); err != nil {
				return err
			}
		}
		if c.credentialReqs != nil {
			if err := c.credentialReqs.StoreSessionCredentialRequirement(admitCtx, sessionID); err != nil {
				return fmt.Errorf("persist session credential requirement: %w", err)
			}
		}
		c.markCredentialedRoot(sessionID)
		return nil
	})
	// Sub-agent spawns inside this turn (the `agent` tool, agentic_fetch)
	// must see the same tenant credentials.
	return c.runInternal(withCallCredentials(ctx, &callCreds), sessionID, prompt, pinned, attachments...)
}

// credentialedRoots methods are coordinator-local sticky state. Membership is
// recorded only after admission and any configured durable marker succeed.
func (c *coordinator) markCredentialedRoot(sessionID string) {
	c.credentialedRootsMu.Lock()
	if c.credentialedRoots == nil {
		c.credentialedRoots = make(map[string]struct{})
	}
	c.credentialedRoots[sessionID] = struct{}{}
	c.credentialedRootsMu.Unlock()
}

func (c *coordinator) isCredentialedRoot(sessionID string) bool {
	c.credentialedRootsMu.Lock()
	defer c.credentialedRootsMu.Unlock()
	_, ok := c.credentialedRoots[sessionID]
	return ok
}

// resolveCallModels resolves a call's model snapshot through its
// per-call CredentialSet when it has one, otherwise through the
// ordinary session path. Single funnel so the initial resolve and
// runInternal's 401 rebuild agree on which credentials a call belongs
// to.
//
// R5-1: when the caller armed per-call model overrides on ctx
// (WithModelOverrides — e.g. the reviewer pass's temporary
// reviewer-model override), a rebuild re-applies them instead of
// falling back to the durable session/config smart slot. Without this,
// a mid-turn 401 followed by a successful credential refresh silently
// changed the model identity mid-reviewer-pass: Reviewer -> 401 ->
// refresh -> Smart. Only the credentials for the SAME already-selected
// provider refresh; the model choice itself survives the rebuild. The
// overrides stay transient: applyModelOverrides reads model
// persistence from ctx, and callers that must not persist (the review
// turn) carry cleared persistence, so the temporary choice is still
// never written over the durable smart slot.
func (c *coordinator) resolveCallModels(ctx context.Context, sessionID string, creds *CredentialSet) (*resolvedOverrides, error) {
	if creds == nil {
		if smartOverride, fastOverride := modelOverridesFrom(ctx); smartOverride != nil || fastOverride != nil {
			return c.applyModelOverrides(ctx, sessionID, smartOverride, fastOverride)
		}
		return c.resolveSessionModels(ctx, sessionID)
	}
	return c.resolveCredentialsModels(ctx, sessionID, creds)
}

// credentialReasoningLevels is the effort ladder assumed for a
// CredentialModel with CanReason set. Per-call credential models carry
// no reasoning-level list of their own, and effectiveReasoningEffort
// only forwards efforts the model claims to accept — without a ladder
// a ModelChoice's ReasoningEffort would be silently dropped. The
// OpenAI-style low/medium/high ladder is the least surprising default.
var credentialReasoningLevels = []string{"low", "medium", "high"}

// applyInvocationEffort gives explicit invocation effort to the selected turn
// model, including credential-selected models when no model override exists.
// Durable session effort is not consulted: it is specific to the session's
// configured model and must not leak onto tenant credentials.
func applyInvocationEffort(ctx context.Context, smart, fast *ModelChoice) {
	effort := roleReasoningEffortFrom(ctx)
	if effort == "" {
		return
	}
	if callModelRole(ctx) == config.SelectedModelTypeFast {
		fast.ReasoningEffort = effort
	} else {
		smart.ReasoningEffort = effort
	}
}

// resolveCredentialsModels builds the per-call model snapshot from
// creds. Roles covered by creds.Models are built ad-hoc (fresh provider
// client, never cached — see buildCredentialModel). Strict isolation is
// the default: with AllowConfiguredRoleFallback false, an uncovered
// smart/fast role errors out HERE — before any session/config resolve
// and before any provider client is built — so tenant data can never
// reach the operator's provider through a missing role. With the flag
// set, uncovered roles fall through to the ordinary resolveSessionModels
// path (a documented boundary crossing).
func (c *coordinator) resolveCredentialsModels(ctx context.Context, sessionID string, creds *CredentialSet) (*resolvedOverrides, error) {
	smartChoice, smartCovered := creds.Models[RoleSmart]
	fastChoice, fastCovered := creds.Models[RoleFast]
	customRole := ""
	customContract := (*session.RoleContract)(nil)
	customCredentialCovered := false
	if opts := callOptionsFrom(ctx); opts != nil && opts.RoleContract != nil && opts.RoleContract.Name != "" {
		customRole = opts.RoleContract.Name
		customContract = opts.RoleContract
		if err := opts.RoleContract.Validate(); err != nil {
			return nil, fmt.Errorf("invalid selected role contract: %w", err)
		}
		if choice, covered := creds.Models[Role(customRole)]; covered {
			smartChoice = choice
			smartCovered = true
			customCredentialCovered = true
		} else if !creds.AllowConfiguredRoleFallback {
			return nil, fmt.Errorf("credential set does not cover requested custom role %q", customRole)
		} else {
			smartCovered = true // operator contract snapshot is the explicit configured fallback
		}
	}
	if smartOverride, fastOverride := modelOverridesFrom(ctx); smartOverride != nil || fastOverride != nil {
		if smartOverride != nil {
			sameModel := smartChoice.Provider == smartOverride.Provider && smartChoice.Model == smartOverride.Model
			smartChoice.Provider = smartOverride.Provider
			smartChoice.Model = smartOverride.Model
			if !sameModel || smartOverride.ReasoningEffort != "" {
				smartChoice.ReasoningEffort = smartOverride.ReasoningEffort
			}
			smartCovered = true
		}
		if fastOverride != nil {
			sameModel := fastChoice.Provider == fastOverride.Provider && fastChoice.Model == fastOverride.Model
			fastChoice.Provider = fastOverride.Provider
			fastChoice.Model = fastOverride.Model
			if !sameModel || fastOverride.ReasoningEffort != "" {
				fastChoice.ReasoningEffort = fastOverride.ReasoningEffort
			}
			fastCovered = true
		}
	}
	applyInvocationEffort(ctx, &smartChoice, &fastChoice)

	if customRole != "" && !customCredentialCovered && !creds.AllowConfiguredRoleFallback {
		return nil, fmt.Errorf("credential set does not cover requested custom role %q", customRole)
	}
	if !smartCovered {
		return nil, fmt.Errorf("credential set does not cover the smart role (Models[RoleSmart]); smart is required even when AllowConfiguredRoleFallback is true")
	}
	if !fastCovered && !creds.AllowConfiguredRoleFallback {
		return nil, fmt.Errorf("credential set does not cover the fast role (Models[RoleFast], drives title generation) and AllowConfiguredRoleFallback is false; add the role or set the flag to serve it from the Client's configured models")
	}

	var base *resolvedOverrides
	if creds.AllowConfiguredRoleFallback && (!fastCovered || (customRole != "" && !customCredentialCovered)) {
		var err error
		base, err = c.resolveSessionModels(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}

	resolved := &resolvedOverrides{credentials: creds}
	resolved.persistSmartModel, resolved.persistFastModel = sessionModelPersistenceFrom(ctx)
	if customContract != nil {
		resolved.smartSource = callbeat.HbSourceSessionOverride
	}
	if base != nil {
		resolved.smart = base.smart
		resolved.fast = base.fast
		resolved.smartSource = base.smartSource
		resolved.fastSource = base.fastSource
		resolved.promptPrefix = base.promptPrefix
		resolved.systemPrompt = base.systemPrompt
		resolved.providerCfg = base.providerCfg
	}

	// One atomic snapshot for the whole resolve (F6): the provider clients
	// built below and the coder toolset at the bottom must read the global
	// network defaults from the SAME generation this resolve runs in, not
	// from a second, later independent read.
	cfg, _ := c.cfg.Snapshot()

	if smartCovered && (customRole == "" || customCredentialCovered) {
		smart, providerCfg, err := c.buildCredentialModel(ctx, cfg, creds, smartChoice)
		if err != nil {
			return nil, err
		}
		resolved.smart = smart
		resolved.smartSource = callbeat.HbSourcePerCall
		resolved.providerCfg = providerCfg
		if err := c.rejectScopedCallOnCLIProvider(ctx, "smart", providerCfg); err != nil {
			return nil, err
		}
		// The credential carries no system-prompt prefix, and the
		// fallback prompt above was built from the CONFIG smart provider:
		// drop both so the turn doesn't style itself for a provider it is
		// not actually using (resolveSessionSystemPrompt still applies
		// the session's own prompt downstream).
		resolved.promptPrefix = ""
		resolved.systemPrompt = ""
	}
	if customRole != "" && !customCredentialCovered && base != nil {
		resolved.smart = base.smart
		resolved.providerCfg = base.providerCfg
		resolved.smartSource = base.smartSource
		resolved.promptPrefix = base.promptPrefix
		resolved.systemPrompt = base.systemPrompt
	}
	if fastCovered {
		fast, _, err := c.buildCredentialModel(ctx, cfg, creds, fastChoice)
		if err != nil {
			return nil, err
		}
		resolved.fast = fast
		resolved.fastSource = callbeat.HbSourcePerCall
	}
	var workerProviderCfg config.ProviderConfig
	var workerConfigured bool
	workerChoice, workerCovered := creds.Models[RoleWorker]
	if !workerCovered {
		// runSubAgent uses the tenant smart choice when the tenant omits
		// worker. Validate that effective fallback, not the unrelated
		// operator worker slot.
		workerChoice = smartChoice
		workerCovered = true
	}
	if workerCovered {
		workerCredential, ok := creds.Credential(workerChoice.Provider)
		if !ok {
			return nil, fmt.Errorf("model choice references provider %q which is not in the credential set", workerChoice.Provider)
		}
		workerProviderCfg = credentialProviderConfig(workerCredential)
		workerConfigured = true
	}
	if workerConfigured {
		if err := c.rejectScopedCallOnCLIProvider(ctx, "worker", workerProviderCfg); err != nil {
			return nil, err
		}
	}

	if customRole != "" && !customCredentialCovered && creds.AllowConfiguredRoleFallback {
		selected, _, modelErr := currentOrSnapshotRoleModel(cfg, customContract)
		if modelErr != nil {
			return nil, modelErr
		}
		model, _, modelErr := c.buildModelsFromCfg(ctx, cfg, selected, selected, false)
		if modelErr != nil {
			return nil, modelErr
		}
		providerCfg, ok := cfg.Providers.Get(selected.Provider)
		if !ok {
			return nil, fmt.Errorf("%w: provider %q for custom role %q", ErrAgentRoleUnconfigured, selected.Provider, customContract.Name)
		}
		resolved.smart = model
		resolved.providerCfg = providerCfg
		resolved.smartSource = callbeat.HbSourceSessionOverride
		resolved.promptPrefix = providerCfg.SystemPromptPrefix
	}

	// resolveSessionModels/applyModelOverrides do, built from THIS call's
	// ctx so buildTools' per-call filters (CallOptions.DisableSubAgents,
	// ModelRole) decide the slice. Deliberately unconditional — also when
	// base != nil: base may have been resolved before this call's
	// CallOptions were attached to ctx, so its tools slice proves nothing
	// about this call's policy; always rebuild from ctx directly.
	// R5-1 (P0)/R6-3 (P1): pinCallTools itself now fails closed (returns a
	// non-nil error) whenever ctx's CallOptions requires a distinct toolset
	// (FolderScope, DiskProvider, DisableSubAgents, or a tool-shaping
	// ModelRole) and the build could not produce one — see
	// ErrScopedCallToolsUnavailable's doc comment (coordinator_models.go).
	tools, err := c.pinCallTools(withCallCredentials(ctx, creds), cfg)
	if err != nil {
		return nil, err
	}
	resolved.tools = tools
	return resolved, nil
}

// buildCredentialModel builds one ad-hoc Model (fresh provider client,
// never cached) plus its provider config from the tenant's credential
// and model choice. Used for every role a call's CredentialSet covers.
// cfg is the caller's pinned config snapshot (F6); it only supplies the
// global network defaults for the provider HTTP client.
func (c *coordinator) buildCredentialModel(ctx context.Context, cfg *config.Config, creds *CredentialSet, choice ModelChoice) (Model, config.ProviderConfig, error) {
	cred, ok := creds.Credential(choice.Provider)
	if !ok {
		return Model{}, config.ProviderConfig{}, fmt.Errorf("model choice references provider %q which is not in the credential set", choice.Provider)
	}

	provCfg := credentialProviderConfig(cred)
	modelCfg := config.SelectedModel{
		Provider:        cred.Provider,
		Model:           choice.Model,
		ReasoningEffort: choice.ReasoningEffort,
		MaxTokens:       choice.MaxTokens,
	}

	provider, err := c.buildProviderWithLiteralCredentials(cfg, provCfg, modelCfg, false)
	if err != nil {
		return Model{}, config.ProviderConfig{}, fmt.Errorf("failed to build provider %q from per-call credentials: %w", cred.Provider, err)
	}

	modelID := choice.Model
	if cred.Type == ProviderTypeOpenRouter && isExactoSupported(modelID) {
		modelID += ":exacto"
	}
	lm, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, config.ProviderConfig{}, fmt.Errorf("failed to build model %q for provider %q: %w", choice.Model, cred.Provider, err)
	}

	return Model{
		Model:      lm,
		CatwalkCfg: credentialCatwalkModel(cred, choice.Model),
		ModelCfg:   modelCfg,
		FlatRate:   provCfg.FlatRate,
	}, provCfg, nil
}

// credentialProviderConfig shapes a Credential into the
// config.ProviderConfig buildProvider consumes. ID carries the
// embedder's provider name so the coordinator's providerCfg.ID gate and
// log lines identify the tenant's provider, not a config one.
func credentialProviderConfig(cred Credential) config.ProviderConfig {
	provCfg := config.ProviderConfig{
		ID:      cred.Provider,
		Name:    cred.Provider,
		Type:    catwalk.Type(cred.Type),
		APIKey:  cred.APIKey,
		BaseURL: cred.BaseURL,
	}
	for _, m := range cred.Models {
		provCfg.Models = append(provCfg.Models, credentialCatwalkModel(cred, m.ID))
	}
	return provCfg
}

// credentialCatwalkModel maps a CredentialModel's metadata onto the
// catwalk.Model shape the Model wrapper carries. A model id with no
// metadata entry degrades to the same unverified-minimal shape
// buildModelsFromCfg synthesizes for unknown rush.json models: zero
// cost/context-window metadata, a warning, no refusal.
func credentialCatwalkModel(cred Credential, modelID string) catwalk.Model {
	for _, m := range cred.Models {
		if m.ID != modelID {
			continue
		}
		cw := catwalk.Model{
			ID:               m.ID,
			Name:             m.ID,
			ContextWindow:    m.ContextWindow,
			DefaultMaxTokens: m.DefaultMaxTokens,
			CanReason:        m.CanReason,
		}
		if m.CanReason {
			cw.ReasoningLevels = credentialReasoningLevels
		}
		return cw
	}
	slog.Warn("per-call credential model has no metadata entry; using unverified minimal metadata (cost/context-window unknown)",
		"provider", cred.Provider, "model", modelID)
	return catwalk.Model{ID: modelID, Name: modelID}
}
