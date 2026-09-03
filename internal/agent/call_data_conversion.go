package agent

import (
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

// toSessionModelCfg converts config.SelectedModel to session.ModelCfg.
// This is needed because session.ModelCfg is a mirror of config.SelectedModel
// without importing the config package (to avoid import cycles).
func toSessionModelCfg(cfg config.SelectedModel) session.ModelCfg {
	return session.ModelCfg{
		Model:            cfg.Model,
		Provider:         cfg.Provider,
		ReasoningEffort:  cfg.ReasoningEffort,
		Think:            cfg.Think,
		MaxTokens:        cfg.MaxTokens,
		Temperature:      cfg.Temperature,
		TopP:             cfg.TopP,
		TopK:             cfg.TopK,
		FrequencyPenalty: cfg.FrequencyPenalty,
		PresencePenalty:  cfg.PresencePenalty,
		ProviderOptions:  cfg.ProviderOptions,
	}
}

// fromSessionModelCfg converts session.ModelCfg to config.SelectedModel.
// This is the inverse of toSessionModelCfg.
func fromSessionModelCfg(cfg session.ModelCfg) config.SelectedModel {
	return config.SelectedModel{
		Model:            cfg.Model,
		Provider:         cfg.Provider,
		ReasoningEffort:  cfg.ReasoningEffort,
		Think:            cfg.Think,
		MaxTokens:        cfg.MaxTokens,
		Temperature:      cfg.Temperature,
		TopP:             cfg.TopP,
		TopK:             cfg.TopK,
		FrequencyPenalty: cfg.FrequencyPenalty,
		PresencePenalty:  cfg.PresencePenalty,
		ProviderOptions:  cfg.ProviderOptions,
	}
}

// toSessionRunAllowlistSpec converts permission.RunAllowlistSpec to its
// session.SessionAgentCallData mirror type (session.RunAllowlistSpec). See
// that type's doc for why the mirror exists.
func toSessionRunAllowlistSpec(spec *permission.RunAllowlistSpec) *session.RunAllowlistSpec {
	if spec == nil {
		return nil
	}
	return &session.RunAllowlistSpec{
		Restrict:   spec.Restrict,
		AllowTools: spec.AllowTools,
		AllowBash:  spec.AllowBash,
	}
}

// fromSessionRunAllowlistSpec is the inverse of toSessionRunAllowlistSpec.
func fromSessionRunAllowlistSpec(spec *session.RunAllowlistSpec) *permission.RunAllowlistSpec {
	if spec == nil {
		return nil
	}
	return &permission.RunAllowlistSpec{
		Restrict:   spec.Restrict,
		AllowTools: spec.AllowTools,
		AllowBash:  spec.AllowBash,
	}
}

// toSessionCallOptionsSpec converts the replay-relevant subset of
// agent.CallOptions to its session.SessionAgentCallData mirror type
// (session.CallOptionsSpec), reading directly off the live CallOptions —
// unlike FolderScopeSpec below, none of these fields need a separate
// pre-compilation form carried through the call's context: they are
// already plain primitives in CallOptions itself. See that type's doc
// for the fields deliberately excluded and why (R5-3).
func toSessionCallOptionsSpec(o *CallOptions) *session.CallOptionsSpec {
	if o == nil {
		return nil
	}
	return &session.CallOptionsSpec{
		Version:                  session.CallOptionsSpecVersion,
		DisableSubAgents:         o.DisableSubAgents,
		ModelRole:                string(o.ModelRole),
		TimeoutOptionsSet:        o.TimeoutOptionsSet,
		TimeoutExtendsOnProgress: o.TimeoutExtendsOnProgress,
		TimeoutHardCap:           o.TimeoutHardCap,
	}
}

// ErrCallOptionsSpecVersionUnsupported is returned by
// fromSessionCallOptionsSpec when a persisted CallOptionsSpec's Version
// does not match the current session.CallOptionsSpecVersion (R6-4, P2
// security review round 6). CallOptionsSpecVersion existed as write-only
// metadata before this fix: toSessionCallOptionsSpec always stamped it,
// but nothing on the read side ever looked at it — fromSessionCallOptionsSpec
// reconstructed CallOptions from whatever fields happened to decode
// regardless of version. During a rollback or a mixed-version deployment,
// an older binary could accept a newer durable row, silently drop an
// unknown execution-policy field, and run the replayed turn with weaker
// semantics than the row actually declared. There is exactly one accepted
// version today (session.CallOptionsSpecVersion); anything else — zero,
// malformed, or a future schema this binary predates — is refused rather
// than partially decoded.
var ErrCallOptionsSpecVersionUnsupported = errors.New("agent: durable CallOptionsSpec has an unsupported schema version")

// fromSessionCallOptionsSpec is the inverse of toSessionCallOptionsSpec. A
// nil spec (no CallOptionsSpec persisted at all — a legacy pre-R5-3 row,
// or a call that carried no CallOptions) returns (nil, nil), preserving
// the historical "fall back to defaults" behavior for that case; it is
// distinct from a non-nil spec with an unrecognized Version, which fails
// closed instead (see ErrCallOptionsSpecVersionUnsupported).
func fromSessionCallOptionsSpec(spec *session.CallOptionsSpec) (*CallOptions, error) {
	if spec == nil {
		return nil, nil
	}
	if spec.Version != session.CallOptionsSpecVersion {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrCallOptionsSpecVersionUnsupported, spec.Version, session.CallOptionsSpecVersion)
	}
	return &CallOptions{
		DisableSubAgents:         spec.DisableSubAgents,
		ModelRole:                config.SelectedModelType(spec.ModelRole),
		TimeoutOptionsSet:        spec.TimeoutOptionsSet,
		TimeoutExtendsOnProgress: spec.TimeoutExtendsOnProgress,
		TimeoutHardCap:           spec.TimeoutHardCap,
	}, nil
}

// toSessionFolderScopeSpec converts permission.FolderScopeSpec to its
// session.SessionAgentCallData mirror type (session.FolderScopeSpec).
// Entries and Ops are copied element-wise because Go cannot convert
// between slices of different named string types in one cast. See the
// mirror type's doc for why it exists.
func toSessionFolderScopeSpec(spec *permission.FolderScopeSpec) *session.FolderScopeSpec {
	if spec == nil {
		return nil
	}
	out := &session.FolderScopeSpec{
		WorkingDir:       spec.WorkingDir,
		KeepCommandTools: spec.KeepCommandTools,
	}
	out.Entries = make([]session.FolderScopeEntry, len(spec.Entries))
	for i, entry := range spec.Entries {
		ops := make([]session.FileOp, len(entry.Ops))
		for j, op := range entry.Ops {
			ops[j] = session.FileOp(op)
		}
		out.Entries[i] = session.FolderScopeEntry{Dir: entry.Dir, Ops: ops}
	}
	return out
}

// fromSessionFolderScopeSpec is the inverse of toSessionFolderScopeSpec.
func fromSessionFolderScopeSpec(spec *session.FolderScopeSpec) *permission.FolderScopeSpec {
	if spec == nil {
		return nil
	}
	out := &permission.FolderScopeSpec{
		WorkingDir:       spec.WorkingDir,
		KeepCommandTools: spec.KeepCommandTools,
	}
	out.Entries = make([]permission.FolderScopeEntry, len(spec.Entries))
	for i, entry := range spec.Entries {
		ops := make([]permission.FileOp, len(entry.Ops))
		for j, op := range entry.Ops {
			ops[j] = permission.FileOp(op)
		}
		out.Entries[i] = permission.FolderScopeEntry{Dir: entry.Dir, Ops: ops}
	}
	return out
}

// ToSessionAgentCallData converts agent.SessionAgentCall to session.SessionAgentCallData
// for durable queue serialization (task #340, ROUND 3 migration).
//
// We only serialize ModelCfg because:
//   - ModelCfg is the per-session pinned snapshot (task #265, P0-1)
//   - ProviderOptions, Temperature, TopP, TopK, FrequencyPenalty, PresencePenalty
//     are pure functions of (Model, ProviderConfig) computed via mergeCallOptions
//     during pump execution ( ROUND 3 design decision).
//   - The live fantasy.LanguageModel and CatwalkCfg in Model are NOT serializable
//     and will be reconstructed by coordinator.RebuildSessionAgentCall.
//
// RunAllowlistSpec round-trips so a pump-driven restart can recompile and re-arm the caller's declared policy (R4-1/R4-2/R4-3).
// FolderScopeSpec round-trips so a pump-driven restart can recompile the call's folder scope (T12).
// CallOptionsSpec round-trips the rest of the call's replay-relevant CallOptions execution policy (DisableSubAgents, ModelRole, timeout-watchdog policy) so a pump-driven restart reconstructs all of it together instead of just the folder scope (R5-3).
//
// LogicalCallID is serialized to ensure the stable idempotency key survives
// the durable serialization boundary (P2-1 fix, P0-1 release blocker).
func ToSessionAgentCallData(call SessionAgentCall) session.SessionAgentCallData {
	var smartModel, fastModel *session.ModelCfg
	if call.SmartModel != nil {
		cfg := toSessionModelCfg(call.SmartModel.ModelCfg)
		smartModel = &cfg
	}
	if call.FastModel != nil {
		cfg := toSessionModelCfg(call.FastModel.ModelCfg)
		fastModel = &cfg
	}

	return session.SessionAgentCallData{
		SessionID:            call.SessionID,
		LogicalCallID:        call.LogicalCallID,
		Prompt:               call.Prompt,
		Attachments:          call.Attachments,
		MaxOutputTokens:      call.MaxOutputTokens,
		NonInteractive:       call.NonInteractive,
		SystemPromptOverride: call.SystemPromptOverride,
		MaxCost:              call.MaxCost,
		MaxTokens:            call.MaxTokens,
		ExistingMessageID:    call.ExistingMessageID,
		InjectID:             call.InjectID,
		SmartModel:           smartModel,
		FastModel:            fastModel,
		SystemPromptPrefix:   call.SystemPromptPrefix,
		SystemPrompt:         call.SystemPrompt,
		Origin:               call.Origin,
		RunAllowlistSpec:     toSessionRunAllowlistSpec(call.RunAllowlistSpec),
		FolderScopeSpec:      toSessionFolderScopeSpec(call.FolderScopeSpec),
		CallOptionsSpec:      toSessionCallOptionsSpec(call.CallOptions),
		// Layer 2 (belt and braces, design doc §7.3): mark the row so a
		// consumer that somehow receives it (a producer added later, or a
		// row written by a different binary version) still fails closed
		// at RebuildSessionAgentCall instead of failing open. Every
		// current producer already refuses to reach this point at all
		// (Layer 1) when this is true.
		HostDiskProvider: callCarriesDiskProvider(call),
	}
}

// FromSessionAgentCallData converts session.SessionAgentCallData back to agent.SessionAgentCall
// for durable queue deserialization (task #340, ROUND 3 migration).
//
// This function only converts the data that was serialized. The full Model
// (with live fantasy.LanguageModel and CatwalkCfg) will be reconstructed by
// coordinator.RebuildSessionAgentCall.
//
// RunAllowlistSpec round-trips so a pump-driven restart can recompile and re-arm the caller's declared policy (R4-1/R4-2/R4-3).
// FolderScopeSpec round-trips so a pump-driven restart can recompile the call's folder scope (T12).
// CallOptionsSpec round-trips into a ready-to-use CallOptions here directly
// (unlike FolderScope, none of its fields need compilation against the
// coordinator's config/disk, so — unlike FolderScopeSpec above, which only
// restores the pre-compilation spec and leaves compiling it to
// coordinator.RebuildSessionAgentCall — this function can fully restore
// CallOptions on its own) (R5-3).
//
// LogicalCallID is restored to ensure the stable idempotency key survives
// the durable serialization boundary (P2-1 fix, P0-1 release blocker).
// callOptionsFromCallData reconstructs CallOptions for FromSessionAgentCallData
// below, which has no error return of its own. This function is a plain
// data-shape converter used only by round-trip tests exercising the OTHER
// fields on SessionAgentCallData (RunAllowlistSpec, FolderScopeSpec,
// LogicalCallID, ...) — it is never called from the production
// durable-restart path, which goes through coordinator.RebuildSessionAgentCall
// instead (that path DOES propagate fromSessionCallOptionsSpec's version
// error and fails the row closed; see its doc comment). An unsupported
// version here simply degrades to nil CallOptions, matching this
// function's existing "best-effort reconstruction, no error channel"
// contract for its test-only callers.
func callOptionsFromCallData(spec *session.CallOptionsSpec) *CallOptions {
	opts, _ := fromSessionCallOptionsSpec(spec)
	return opts
}

func FromSessionAgentCallData(callData session.SessionAgentCallData) SessionAgentCall {
	return SessionAgentCall{
		SessionID:            callData.SessionID,
		LogicalCallID:        callData.LogicalCallID,
		Prompt:               callData.Prompt,
		Attachments:          callData.Attachments,
		MaxOutputTokens:      callData.MaxOutputTokens,
		NonInteractive:       callData.NonInteractive,
		SystemPromptOverride: callData.SystemPromptOverride,
		MaxCost:              callData.MaxCost,
		MaxTokens:            callData.MaxTokens,
		ExistingMessageID:    callData.ExistingMessageID,
		InjectID:             callData.InjectID,
		SystemPromptPrefix:   callData.SystemPromptPrefix,
		SystemPrompt:         callData.SystemPrompt,
		Origin:               callData.Origin,
		RunAllowlistSpec:     fromSessionRunAllowlistSpec(callData.RunAllowlistSpec),
		FolderScopeSpec:      fromSessionFolderScopeSpec(callData.FolderScopeSpec),
		CallOptions:          callOptionsFromCallData(callData.CallOptionsSpec),
		// SmartModel and FastModel are NOT set here — they will be reconstructed
		// by coordinator.RebuildSessionAgentCall.
	}
}
