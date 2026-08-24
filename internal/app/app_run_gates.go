// `crush run` gating applied before the agent starts: the default
// sub-agent tool ban and its smart+worker bypass, plus the
// restricted-run allowlist spec derived from config permissions.

package app

import (
	"slices"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
)

// subAgentToolNames lists every tool name that the default `crush run`
// sub-agent ban strips from the coder agent's AllowedTools. Split out so
// callers that want to restore a subset (see the smart+worker bypass in
// RunNonInteractive) can strip everything EXCEPT that subset instead of
// duplicating the loop in disableSubAgentToolsInConfig.
var subAgentToolNames = []string{"agent", "agentic_fetch"}

// disableSubAgentToolsInConfig drops the given tool names from the coder
// agent's AllowedTools list in the in-memory config. Used by
// RunNonInteractive when overrides.DisableSubAgents (`crush run --agents
// single`, or the implicit default when --agents is unset) is set.
// Mutation does not touch the on-disk config and only outlives this
// process if a future caller reloads the in-memory config from disk —
// `crush run` exits immediately after the agent turn so this is moot in
// practice.
//
// Fork patch (orchestrator UX): see CHANGELOG.fork.md (Section 4.J).
func (app *App) disableSubAgentToolsInConfig() {
	app.disableToolsInConfig(subAgentToolNames)
}

// disableToolsInConfig drops exactly the named tools from the coder
// agent's AllowedTools list in the in-memory config. Extracted so the
// smart+worker bypass (see shouldBypassSubAgentBan) can restore just the
// `agent` tool while keeping `agentic_fetch` stripped, without
// duplicating the filter loop.
//
// Reads the currently-published config via Config() (read-only, per its
// documented contract) to compute the filtered tool list, then applies the
// change through ConfigStore.UpdateAgentAllowedTools, which publishes it as
// a new generation via the copy-on-write path instead of writing directly
// into cfg.Agents[...] on the already-published *Config — see
// UpdateAgentAllowedTools's doc comment for why that in-place write was a
// contract violation (a concurrent reader holding the old *Config would see
// the mutation retroactively).
//
// Fork patch (orchestrator UX, plan phase 2): bypass restores `agent` only.
func (app *App) disableToolsInConfig(toolNames []string) {
	cfg := app.config.Config()
	if cfg == nil {
		return
	}
	coder, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return
	}
	filtered := coder.AllowedTools[:0:0]
	for _, t := range coder.AllowedTools {
		if slices.Contains(toolNames, t) {
			continue
		}
		filtered = append(filtered, t)
	}
	app.config.UpdateAgentAllowedTools(config.AgentCoder, filtered)
}

// shouldBypassSubAgentBan decides whether the `crush run` default
// sub-agent ban (DisableSubAgents) should be bypassed for the `agent`
// tool specifically, restoring it to the coder's AllowedTools even
// though the ban is otherwise in effect.
//
// Fork patch (orchestrator UX, plan phase 2): the orchestrator design (a smart
// parent delegating hands-on work to cheap worker sub-agents) depends on
// the `agent` tool being available. The ban exists to stop an
// unsupervised `crush run` from silently fanning out — but when a Worker
// model is configured, that fan-out IS the point, so the ban would
// otherwise block the very feature it was configured for. This applies
// regardless of whether `--agents single` was passed explicitly or left
// unset: a configured worker means the operator's intent is delegation,
// and `--agents single` alone (with no worker unconfigured to back it)
// is not a strong enough signal to override that.
//
// Bypasses only when ALL of:
//   - role == SelectedModelTypeLarge: the run declared --role smart.
//     --role fast (or worker/reviewer) never bypasses; we don't
//     second-guess an explicit non-smart role choice.
//   - a Worker model slot is configured with a non-empty Model. No
//     worker means there is nothing productive for the `agent` tool to
//     delegate to, so the historical (safe) default — sub-agents off —
//     stands. This is the single most important case: worker NOT
//     configured + role smart + flags unset must keep sub-agents
//     disabled exactly as today.
func shouldBypassSubAgentBan(role config.SelectedModelType, cfg *config.Config) bool {
	if role != config.SelectedModelTypeSmart {
		return false
	}
	if cfg == nil {
		return false
	}
	workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
	return ok && workerModelCfg.Model != ""
}

// runAllowlistSpecFromConfig reads the config-derived restricted-run
// allowlist spec (pre-compilation). Returns an inert spec (Restrict =
// false) when permissions.run is absent or disabled, preserving the
// legacy `crush run` auto-approve-everything behaviour.
func runAllowlistSpecFromConfig(p *config.Permissions) permission.RunAllowlistSpec {
	spec := permission.RunAllowlistSpec{}
	if p == nil || p.Run == nil {
		return spec
	}
	spec.Restrict = p.Run.Restrict
	// Defensive copies so a later config reload can't mutate the spec
	// we hand to the compiler.
	spec.AllowTools = append(spec.AllowTools, p.Run.AllowTools...)
	spec.AllowBash = append(spec.AllowBash, p.Run.AllowBash...)
	return spec
}
