// Fork patch: batch 11 — `rush models state` shows the effective smart/fast
// pair, the scope each came from, and a per-scope breakdown of what is written
// to disk. Replaces the implicit story (`models show` alone doesn't say WHERE
// each slot came from).
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var modelsStateCmd = &cobra.Command{
	Use:     "state",
	Aliases: []string{"show"}, // backwards-compat: `rush models show` used to exist.
	Short:   "Show what's currently effective and from which scope",
	Long: `Print three things:
  1. EFFECTIVE — the (smart, fast, worker, reviewer) values that
     ` + "`rush run --role smart/fast/worker/reviewer`" + ` will actually use, and
     which scope each came from. worker and reviewer are optional; when unset
     in both scopes they print "(not set in any scope)".
  2. SCOPES — what each scope (global, local) says about each slot, with
     "(effective)" / "(overridden by local)" / "(not set)" annotations.
  3. The atom name in parens when the effective model matches a known atom.
  4. For a slot with no explicit effort, the known unset-default (e.g.
     "unset -> thinking on, high" for Z.AI) — silent when undocumented.

Set worker/reviewer with ` + "`rush models use <smart> <fast> --worker <m> --reviewer <m>`" + `
and clear them with ` + "`rush models unset worker`" + ` / ` + "`rush models unset reviewer`" + `.

` + "`--json`" + ` emits a structured object for orchestrators.`,
	Example: `
# Plain text: effective pair + scope breakdown.
rush models state

# Machine-readable for orchestrators (jq-friendly):
rush models state --json | jq '.effective'

# After changing the workspace override, see what's now effective:
rush models use --local opus-high glm5_turbo && rush models state

# Backwards-compat alias of the same command:
rush models show
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		cfg := a.Config()
		store := a.Store()

		globalAll, gerr := store.ReadAllModelsAtScope(config.ScopeGlobal)
		if gerr != nil {
			return fmt.Errorf("read global scope: %w", gerr)
		}
		localAll, lerr := store.ReadAllModelsAtScope(config.ScopeWorkspace)
		if lerr != nil {
			return fmt.Errorf("read local scope: %w", lerr)
		}

		globalSmart, globalFast := globalAll[config.SelectedModelTypeSmart], globalAll[config.SelectedModelTypeFast]
		localSmart, localFast := localAll[config.SelectedModelTypeSmart], localAll[config.SelectedModelTypeFast]
		globalWorker, globalReviewer := globalAll[config.SelectedModelTypeWorker], globalAll[config.SelectedModelTypeReviewer]
		localWorker, localReviewer := localAll[config.SelectedModelTypeWorker], localAll[config.SelectedModelTypeReviewer]

		effSmart, hasSmart := cfg.Models[config.SelectedModelTypeSmart]
		effFast, hasFast := cfg.Models[config.SelectedModelTypeFast]
		effWorker, hasWorker := cfg.Models[config.SelectedModelTypeWorker]
		effReviewer, hasReviewer := cfg.Models[config.SelectedModelTypeReviewer]

		smartScope := whichScope(localSmart, globalSmart)
		fastScope := whichScope(localFast, globalFast)
		workerScope := whichScope(localWorker, globalWorker)
		reviewerScope := whichScope(localReviewer, globalReviewer)

		if asJSON {
			payload := map[string]any{
				"effective": map[string]any{
					"smart":                   nilOrModel(hasSmart, effSmart),
					"fast":                    nilOrModel(hasFast, effFast),
					"worker":                  nilOrModel(hasWorker, effWorker),
					"reviewer":                nilOrModel(hasReviewer, effReviewer),
					"smart_scope":             smartScope,
					"fast_scope":              fastScope,
					"worker_scope":            workerScope,
					"reviewer_scope":          reviewerScope,
					"smart_effort_default":    nilOrEffortDefault(hasSmart, effSmart),
					"fast_effort_default":     nilOrEffortDefault(hasFast, effFast),
					"worker_effort_default":   nilOrEffortDefault(hasWorker, effWorker),
					"reviewer_effort_default": nilOrEffortDefault(hasReviewer, effReviewer),
					"smart_effort_stale":      nilOrStaleEffort(hasSmart, effSmart),
					"fast_effort_stale":       nilOrStaleEffort(hasFast, effFast),
					"worker_effort_stale":     nilOrStaleEffort(hasWorker, effWorker),
					"reviewer_effort_stale":   nilOrStaleEffort(hasReviewer, effReviewer),
				},
				"global": map[string]any{
					"smart":    globalSmart,
					"fast":     globalFast,
					"worker":   globalWorker,
					"reviewer": globalReviewer,
				},
				"local": map[string]any{
					"smart":    localSmart,
					"fast":     localFast,
					"worker":   localWorker,
					"reviewer": localReviewer,
				},
			}
			return json.NewEncoder(os.Stdout).Encode(payload)
		}

		fmt.Println("EFFECTIVE")
		printEffectiveLine("smart", hasSmart, effSmart, smartScope)
		printEffectiveLine("fast", hasFast, effFast, fastScope)
		printEffectiveLine("worker", hasWorker, effWorker, workerScope)
		printEffectiveLine("reviewer", hasReviewer, effReviewer, reviewerScope)
		fmt.Println()
		fmt.Println("SCOPES")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		printScopeLine(tw, "global", "smart", globalSmart, localSmart, "global")
		printScopeLine(tw, "global", "fast", globalFast, localFast, "global")
		printScopeLine(tw, "global", "worker", globalWorker, localWorker, "global")
		printScopeLine(tw, "global", "reviewer", globalReviewer, localReviewer, "global")
		printScopeLine(tw, "local", "smart", localSmart, globalSmart, "local")
		printScopeLine(tw, "local", "fast", localFast, globalFast, "local")
		printScopeLine(tw, "local", "worker", localWorker, globalWorker, "local")
		printScopeLine(tw, "local", "reviewer", localReviewer, globalReviewer, "local")
		tw.Flush()
		return nil
	},
}

func whichScope(local, global *config.SelectedModel) string {
	if local != nil {
		return "local"
	}
	if global != nil {
		return "global"
	}
	return ""
}

func nilOrModel(has bool, m config.SelectedModel) any {
	if !has {
		return nil
	}
	return m
}

// nilOrEffortDefault is the JSON counterpart of effortEffectiveNote: null
// when the slot is unset, when the slot's effort is explicitly set (nothing
// to report — the model's own reasoning_effort field already says so), or
// when the provider's unset-default behavior isn't documented. Otherwise the
// same short fact effortEffectiveNote renders in text (without the
// parens/leading space, since JSON consumers don't need the human framing).
func nilOrEffortDefault(has bool, m config.SelectedModel) any {
	if !has || m.ReasoningEffort != "" {
		return nil
	}
	if note := unsetEffortNote(m.Provider); note != "" {
		return note
	}
	return nil
}

// nilOrStaleEffort is the JSON counterpart of staleEffortNote: null when the
// slot is unset or its effort is still valid, otherwise the levels that ARE
// valid, so an orchestrator can detect and repair a stale config without
// re-deriving the atom's vocabulary itself.
func nilOrStaleEffort(has bool, m config.SelectedModel) any {
	if !has || staleEffortNote(m) == "" {
		return nil
	}
	return atomRegistry[lookupAtomForModel(m)].Levels()
}

func printEffectiveLine(label string, has bool, m config.SelectedModel, scope string) {
	if !has {
		fmt.Printf("  %s:  (not set in any scope)\n", label)
		return
	}
	atomLabel := ""
	if k := lookupAtomForModel(m); k != "" {
		// The "<atom>-<effort>" form is only printed when that exact string
		// would still be accepted by `rush models use`. A stale effort (see
		// staleEffortNote) falls back to the bare atom key so the output
		// never suggests a command the CLI now rejects.
		if m.ReasoningEffort != "" && staleEffortNote(m) == "" {
			atomLabel = fmt.Sprintf(" (atom: %s-%s)", k, m.ReasoningEffort)
		} else {
			atomLabel = fmt.Sprintf(" (atom: %s)", k)
		}
	}
	src := scope
	switch scope {
	case "global":
		src = "from GLOBAL"
	case "local":
		src = "from LOCAL"
	default:
		src = "scope unknown"
	}
	fmt.Printf("  %s:  %s/%s%s%s%s   (%s)\n",
		label, m.Provider, m.Model, effortSuffix(m.ReasoningEffort), atomLabel, effortEffectiveNote(m), src)
}

// effortEffectiveNote returns a terse parenthetical (with a leading space,
// or "" when there's nothing to add) for a slot's effort state: nothing
// when an effort is explicitly set (effortSuffix above already shows that),
// and the documented unset-default fact — reusing unsetEffortNote from
// models_efforts.go so this can never drift from `rush models efforts`'s
// prose or coordinator.go's actual switch — when the effort is unset and
// that provider's default is known. Silent ("") for unset effort on a
// provider whose default isn't documented; never guesses.
func effortEffectiveNote(m config.SelectedModel) string {
	if m.ReasoningEffort != "" {
		return staleEffortNote(m)
	}
	if note := unsetEffortNote(m.Provider); note != "" {
		return "  (" + note + ")"
	}
	return ""
}

// staleEffortNote flags a stored effort that is no longer one of its atom's
// declared levels — a config written before that atom's level vocabulary
// changed (e.g. GLM-5.3/5.3-Flash dropped off/on for low/high/max once Z.AI
// stopped allowing reasoning to be disabled; see zai53ReasoningLevels in
// models_atoms.go). Such a value still reaches the provider — the wire
// mapping in coordinator_providers.go folds an unrecognized effort onto a
// sane default rather than failing — but `rush models use <atom>-<effort>`
// would now reject it, so printing it as a live atom suffix would be a lie.
// Returns "" when the effort is valid, unset, or the model isn't a known
// atom (efforts on non-atom models stay deliberately unvalidated).
//
// Deliberately checks the atom's static ReasoningLevels directly rather than
// calling validateEffortForModel: that helper also serves Claude/local-cli
// atoms, whose Levels() shells out to `claude --help` (internal/cmd/
// models_effort.go). `rush models state` is a read-only status command that
// must stay fast and work with no `claude` binary installed — it must never
// spawn a subprocess as a side effect of rendering a line, and a CLI-detected
// vocabulary is machine-state-dependent anyway (not the kind of durable
// "declaration changed" fact this note exists to report).
func staleEffortNote(m config.SelectedModel) string {
	if m.ReasoningEffort == "" {
		return ""
	}
	key := lookupAtomForModel(m)
	if key == "" {
		return ""
	}
	a := atomRegistry[key]
	if a.EffortSource != nil {
		return ""
	}
	levels := a.ReasoningLevels
	if levels == nil {
		return ""
	}
	for _, l := range levels {
		if l == m.ReasoningEffort {
			return ""
		}
	}
	return fmt.Sprintf("  (STALE: %q is no longer valid here — valid: %s; re-set with `rush models use`)",
		m.ReasoningEffort, strings.Join(levels, "|"))
}

func printScopeLine(tw *tabwriter.Writer, scopeName, slot string, value, other *config.SelectedModel, ownScope string) {
	if value == nil {
		fmt.Fprintf(tw, "  %s\t%s = —\t(not set)\n", scopeName, slot)
		return
	}
	annotation := ""
	switch {
	case ownScope == "global" && other != nil:
		annotation = "(overridden by local)"
	case ownScope == "global":
		annotation = "(effective)"
	case ownScope == "local":
		annotation = "(effective)"
	}
	fmt.Fprintf(tw, "  %s\t%s = %s/%s%s%s\t%s\n",
		scopeName, slot, value.Provider, value.Model, effortSuffix(value.ReasoningEffort), effortEffectiveNote(*value), annotation)
}

func init() {
	modelsStateCmd.Flags().Bool("json", false, "Emit a structured JSON object")
	modelsCmd.AddCommand(modelsStateCmd)
}
