// Fork patch: batch 13 — `rush models unset [smart|fast|both] [--global|--local]`
// removes a model override from the chosen scope so the other scope takes
// effect, without having to hand-edit rush.json or `rm` the whole file.
package cmd

import (
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var modelsUnsetCmd = &cobra.Command{
	Use:   "unset [smart|fast|worker|reviewer|both|all]",
	Short: "Remove a model override from the chosen scope (defaults to smart+fast, global scope)",
	Long: `Delete the models.<slot> entry (or entries) from the chosen scope's
rush.json so the OTHER scope's value becomes effective again.

Positional arg (optional):
  smart     — only the smart slot
  fast      — only the fast slot
  worker    — only the optional worker slot
  reviewer  — only the optional reviewer slot
  both      — smart + fast (default if omitted; matches ` + "`rush models use`" + `'s scope)
  all       — all four slots, including worker/reviewer

Scope flags (mutually exclusive):
  --global  (default) ~/.local/share/rush/rush.json
  --local             ./.rush/rush.json

Missing keys are a no-op (exit 0). After the deletion, an empty
"models" object is also stripped so the file stays clean.`,
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: []string{"smart", "fast", "worker", "reviewer", "both", "all"},
	Example: `
# Clear the smart+fast workspace override so the global config takes effect again.
rush models unset --local

# Same but globally — wipes smart+fast from ~/.local/share/rush/rush.json.
rush models unset --global

# Drop just the smart slot in the workspace; keep the fast one.
rush models unset smart --local

# Drop just the fast slot globally.
rush models unset fast --global

# Clear the worker slot globally (falls back to no worker — sub-agents use smart).
rush models unset worker --global

# Clear the reviewer slot in the workspace.
rush models unset reviewer --local

# Clear all four slots (smart, fast, worker, reviewer) globally.
rush models unset all --global

# Confirm what survived:
rush models state
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		which := "both"
		if len(args) == 1 {
			which = args[0]
		}
		switch which {
		case "smart", "fast", "worker", "reviewer", "both", "all":
			// ok
		default:
			return fmt.Errorf("unexpected positional %q — expected smart|fast|worker|reviewer|both|all", which)
		}

		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		store := a.Store()

		// Snapshot prior values so we can show what was unset.
		priorAll, _ := store.ReadAllModelsAtScope(scope)

		targets := []struct {
			label string
			key   string
			prior *config.SelectedModel
		}{
			{"smart", "models.smart", priorAll[config.SelectedModelTypeSmart]},
			{"fast", "models.fast", priorAll[config.SelectedModelTypeFast]},
			{"worker", "models.worker", priorAll[config.SelectedModelTypeWorker]},
			{"reviewer", "models.reviewer", priorAll[config.SelectedModelTypeReviewer]},
		}

		// "both" (the default) only ever touched smart+fast; "all" is the new
		// spelling for every slot including worker/reviewer.
		selected := func(label string) bool {
			switch which {
			case "both":
				return label == "smart" || label == "fast"
			case "all":
				return true
			default:
				return which == label
			}
		}

		didDelete := false
		for _, t := range targets {
			if !selected(t.label) {
				continue
			}
			if t.prior == nil {
				fmt.Fprintf(os.Stderr, "%s was not set in %s scope (no-op)\n", t.label, scope)
				continue
			}
			if err := store.RemoveConfigField(scope, t.key); err != nil {
				return fmt.Errorf("failed to unset %s in %s scope: %w", t.label, scope, err)
			}
			fmt.Fprintf(os.Stderr, "unset %s in %s scope (was %s/%s%s)\n",
				t.label, scope, t.prior.Provider, t.prior.Model, effortSuffix(t.prior.ReasoningEffort))
			didDelete = true
		}

		// If we just emptied the `models` object, strip it so the scope file
		// doesn't sit as `"models": {}`. Best-effort: if the read or write
		// fails, do not surface the error — the field-level unset already
		// succeeded and that's what the user asked for.
		if didDelete {
			postAll, perr := store.ReadAllModelsAtScope(scope)
			if perr == nil && len(postAll) == 0 {
				_ = store.RemoveConfigField(scope, "models")
			}
		}

		return nil
	},
}

func init() {
	modelsUnsetCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsUnsetCmd.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	modelsUnsetCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsCmd.AddCommand(modelsUnsetCmd)
}
