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
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
)

// ProviderType mirrors catwalk.Type (charm.land/catwalk,
// pkg/catwalk/provider.go) one-to-one: a fixed closed enum, not a
// free-form string. Keep in sync with catwalk.Type.
type ProviderType string

const (
	ProviderTypeOpenAI       ProviderType = "openai"
	ProviderTypeOpenAICompat ProviderType = "openai-compat"
	ProviderTypeOpenRouter   ProviderType = "openrouter"
	ProviderTypeAnthropic    ProviderType = "anthropic"
	ProviderTypeGoogle       ProviderType = "google"
	ProviderTypeGoogleVertex ProviderType = "google-vertex"
	ProviderTypeAzure        ProviderType = "azure"
	ProviderTypeBedrock      ProviderType = "bedrock"
	ProviderTypeVercel       ProviderType = "vercel"
)

// Role names the model slot a ModelChoice fills in a CredentialSet. The
// string values MUST stay literally identical to
// config.SelectedModelType* so a Role casts to config.SelectedModelType
// (and back) without conversion.
type Role string

const (
	RoleSmart    Role = "smart"
	RoleFast     Role = "fast"
	RoleWorker   Role = "worker"
	RoleReviewer Role = "reviewer"
)

// validRoles and validProviderTypes list the enum values in display
// order for validation errors; the known* helpers are Validate's
// membership checks. Keep both slices in sync with the const blocks
// above.
var (
	validRoles         = []Role{RoleSmart, RoleFast, RoleWorker, RoleReviewer}
	validProviderTypes = []ProviderType{
		ProviderTypeOpenAI,
		ProviderTypeOpenAICompat,
		ProviderTypeOpenRouter,
		ProviderTypeAnthropic,
		ProviderTypeGoogle,
		ProviderTypeGoogleVertex,
		ProviderTypeAzure,
		ProviderTypeBedrock,
		ProviderTypeVercel,
	}
)

// knownRole reports whether role is one of the four Role constants.
func knownRole(role Role) bool {
	for _, known := range validRoles {
		if role == known {
			return true
		}
	}
	return false
}

// knownProviderType reports whether t is one of the nine ProviderType
// constants.
func knownProviderType(t ProviderType) bool {
	for _, known := range validProviderTypes {
		if t == known {
			return true
		}
	}
	return false
}

// roleNames renders validRoles as a comma-separated list for error
// messages.
func roleNames() string {
	names := make([]string, 0, len(validRoles))
	for _, role := range validRoles {
		names = append(names, string(role))
	}
	return strings.Join(names, ", ")
}

// providerTypeNames renders validProviderTypes as a comma-separated
// list for error messages.
func providerTypeNames() string {
	names := make([]string, 0, len(validProviderTypes))
	for _, t := range validProviderTypes {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

// CredentialModel describes one model a tenant's provider serves. Pure
// metadata: Rush never validates a ModelChoice.Model against the owning
// Credential's list — the first real provider call is the validator,
// exactly like `rush run --model` today. Named CredentialModel rather
// than Model because package agent already declares Model for a built
// fantasy.LanguageModel wrapper; the JSON shape is the contract, not
// the Go name.
type CredentialModel struct {
	ID               string `json:"id"`
	ContextWindow    int64  `json:"context_window,omitempty"`
	DefaultMaxTokens int64  `json:"default_max_tokens,omitempty"`
	CanReason        bool   `json:"can_reason,omitempty"`
}

// Credential is one tenant provider: which API shape it speaks (Type),
// where to reach it, and the literal API key to present. API-key only by
// design — OAuth/token-refresh providers are explicitly out of scope
// for per-call credentials. The Models list is informational metadata
// (context window, max tokens, reasoning), never a validation allowlist.
type Credential struct {
	Provider string            `json:"provider"`
	Type     ProviderType      `json:"type"`
	APIKey   string            `json:"api_key"`
	BaseURL  string            `json:"base_url,omitempty"`
	Models   []CredentialModel `json:"models,omitempty"`
}

// ModelChoice selects which model of which provider serves one role for
// this call. Model is NOT validated against the owning Credential's
// Models list — an unknown id degrades exactly like an unverified model
// in rush.json (zero-value metadata, warning log) and fails on the
// first real provider call if truly wrong.
type ModelChoice struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	MaxTokens       int64  `json:"max_tokens,omitempty"`
}

// CredentialSet is the per-call credential bundle for
// RunWithCredentials. The smart role is required in Models; fast,
// worker, and reviewer are optional. Strict isolation is the default:
// a smart/fast role the set does not cover is a hard error before any
// provider traffic — Rush will NOT silently serve it from the Client's
// configured providers. Set AllowConfiguredRoleFallback to explicitly
// re-enable configured-model fallback for uncovered roles (see that
// field's doc for the trust boundary it crosses). Treat the value as
// immutable after construction; a RunWithCredentials call never
// mutates it.
type CredentialSet struct {
	Credentials []Credential         `json:"credentials"`
	Models      map[Role]ModelChoice `json:"models"`
	// AllowConfiguredRoleFallback re-enables the ordinary session/config
	// model resolution for roles this set does not cover. The default
	// (false) is fail-closed: an uncovered smart/fast role is an error
	// before any provider traffic. True deliberately crosses the
	// tenant-credential boundary: the uncovered role — with whatever
	// tenant data it carries (fast drives title generation) — is served
	// by the Client's configured (operator) provider, not the tenant's.
	AllowConfiguredRoleFallback bool `json:"allow_configured_role_fallback,omitempty"`
}

// Validate checks the bundle's internal consistency: at least one
// credential, at least one role choice, every choice naming a known
// credential, non-empty provider types drawn from the nine ProviderType
// constants, a base URL wherever the provider type has no built-in
// default endpoint, Models keys restricted to the four Role constants,
// and the required smart role present. Model IDs are deliberately NOT
// checked against the Credentials' Models lists.
func (cs *CredentialSet) Validate() error {
	if cs == nil {
		return fmt.Errorf("credential set is nil")
	}
	if len(cs.Credentials) == 0 {
		return fmt.Errorf("credential set has no credentials")
	}
	if len(cs.Models) == 0 {
		return fmt.Errorf("credential set has no model choices")
	}
	known := make(map[string]struct{}, len(cs.Credentials))
	for _, cred := range cs.Credentials {
		if cred.Provider == "" {
			return fmt.Errorf("credential is missing its provider name")
		}
		if _, dup := known[cred.Provider]; dup {
			return fmt.Errorf("duplicate credential for provider %q", cred.Provider)
		}
		known[cred.Provider] = struct{}{}
		if cred.Type == "" {
			return fmt.Errorf("credential for provider %q is missing its type", cred.Provider)
		}
		if !knownProviderType(cred.Type) {
			return fmt.Errorf("credential for provider %q has unknown type %q; valid types: %s", cred.Provider, cred.Type, providerTypeNames())
		}
		if cred.Type == ProviderTypeOpenAICompat && cred.BaseURL == "" {
			return fmt.Errorf("provider %q: base_url is required for the %q provider type", cred.Provider, cred.Type)
		}
	}
	for role, choice := range cs.Models {
		if !knownRole(role) {
			return fmt.Errorf("model choice uses unknown role %q; valid roles: %s", role, roleNames())
		}
		if _, ok := known[choice.Provider]; !ok {
			return fmt.Errorf("model choice for role %q references provider %q which is not in the credential set", role, choice.Provider)
		}
		if choice.Model == "" {
			return fmt.Errorf("model choice for role %q has an empty model id", role)
		}
	}
	if _, ok := cs.Models[RoleSmart]; !ok {
		return fmt.Errorf("credential set must define the smart role (Models[RoleSmart]); smart drives every turn")
	}
	return nil
}

// credential returns the named credential from the set.
func (cs *CredentialSet) credential(provider string) (Credential, bool) {
	for _, cred := range cs.Credentials {
		if cred.Provider == provider {
			return cred, true
		}
	}
	return Credential{}, false
}

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
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}
	if creds == nil {
		return nil, fmt.Errorf("RunWithCredentials: credentials are required")
	}
	if err := creds.Validate(); err != nil {
		return nil, fmt.Errorf("RunWithCredentials: %w", err)
	}

	pinned, err := c.resolveCredentialsModels(ctx, sessionID, creds)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve models from per-call credentials: %w", err)
	}

	// Sub-agent spawns inside this turn (the `agent` tool, agentic_fetch)
	// must see the same tenant credentials.
	return c.runInternal(withCallCredentials(ctx, creds), sessionID, prompt, pinned, attachments...)
}

// resolveCallModels resolves a call's model snapshot through its
// per-call CredentialSet when it has one, otherwise through the
// ordinary session path. Single funnel so the initial resolve and
// runInternal's 401 rebuild agree on which credentials a call belongs
// to.
func (c *coordinator) resolveCallModels(ctx context.Context, sessionID string, creds *CredentialSet) (*resolvedOverrides, error) {
	if creds == nil {
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

	if !smartCovered {
		return nil, fmt.Errorf("credential set does not cover the smart role (Models[RoleSmart]) and AllowConfiguredRoleFallback is false; smart drives every turn, so there is no safe fallback")
	}
	if !fastCovered && !creds.AllowConfiguredRoleFallback {
		return nil, fmt.Errorf("credential set does not cover the fast role (Models[RoleFast], drives title generation) and AllowConfiguredRoleFallback is false; add the role or set the flag to serve it from the Client's configured models")
	}

	var base *resolvedOverrides
	if creds.AllowConfiguredRoleFallback && (!smartCovered || !fastCovered) {
		var err error
		base, err = c.resolveSessionModels(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}

	resolved := &resolvedOverrides{credentials: creds}
	if base != nil {
		resolved.smart = base.smart
		resolved.fast = base.fast
		resolved.promptPrefix = base.promptPrefix
		resolved.systemPrompt = base.systemPrompt
		resolved.providerCfg = base.providerCfg
	}

	if smartCovered {
		smart, providerCfg, err := c.buildCredentialModel(ctx, creds, smartChoice)
		if err != nil {
			return nil, err
		}
		resolved.smart = smart
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
	if fastCovered {
		fast, _, err := c.buildCredentialModel(ctx, creds, fastChoice)
		if err != nil {
			return nil, err
		}
		resolved.fast = fast
	}

	cfg, _ := c.cfg.Snapshot()
	// F1: pin THIS call's coder toolset exactly like
	// resolveSessionModels/applyModelOverrides do, built from THIS call's
	// ctx so buildTools' per-call filters (CallOptions.DisableSubAgents,
	// ModelRole) decide the slice. Deliberately unconditional — also when
	// base != nil: base may have been resolved before this call's
	// CallOptions were attached to ctx, so its tools slice proves nothing
	// about this call's policy; always rebuild from ctx directly.
	resolved.tools = c.pinCallTools(ctx, cfg)
	// R5-1 (P0): a nil result for a scoped/provider-backed call must fail
	// the call, not silently fall back to the shared unscoped toolset — see
	// ErrScopedCallToolsUnavailable's doc comment (coordinator_models.go).
	if resolved.tools == nil && scopedCallToolsRequired(ctx) {
		return nil, ErrScopedCallToolsUnavailable
	}
	return resolved, nil
}

// buildCredentialModel builds one ad-hoc Model (fresh provider client,
// never cached) plus its provider config from the tenant's credential
// and model choice. Used for every role a call's CredentialSet covers.
func (c *coordinator) buildCredentialModel(ctx context.Context, creds *CredentialSet, choice ModelChoice) (Model, config.ProviderConfig, error) {
	cred, ok := creds.credential(choice.Provider)
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

	provider, err := c.buildProvider(provCfg, modelCfg, false)
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
