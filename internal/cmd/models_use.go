// Fork patch: batch 11 — `rush models use <smart> <fast>` replaces the older
// `rush models set --smart X --fast Y` with positional args + atom registry.
package cmd

import (
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var modelsUseCmd = &cobra.Command{
	Use:   "use [<smart> <fast>]",
	Short: "Set any of the four model slots from atom names — all at once or one at a time",
	Long: `Activate model slots using the atom syntax. Each value is either an atom
name (e.g. "opus-high", "glm5_turbo") OR a raw "provider/model[@level]"
string for models not in the atom registry.

The chosen scope is written to rush.json:
  --global (default)  ~/.local/share/rush/rush.json
  --local             ./.rush/rush.json

The current value in the OTHER scope is preserved; effective resolution
remains "local if set, else global".

Two forms are supported — never mix them in the same call:

  1. Positional (sets smart + fast together):
       rush models use <smart> <fast>

  2. Flags (set any subset of the four slots independently — e.g. only the
     fast model, leaving smart/worker/reviewer untouched):
       rush models use --fast <atom>
       rush models use --smart <atom> --reviewer <atom>

--worker and --reviewer (see ` + "`rush models --help`" + ` for what each is for)
are ALWAYS flag-only, in both forms, and are resolved/written independently
of smart/fast. Omit a flag to leave that slot untouched.

Every one of the four slots accepts an effort suffix right here — either
"<atom>-<level>" (e.g. "glm5_3-max") or "provider/model@level" — no separate
step needed. See ` + "`rush models efforts [model]`" + ` to list the levels a model
supports, and ` + "`rush models bump <role> up|down`" + ` to nudge an already-set
effort later.

See ` + "`rush models list`" + ` for the full atom table.`,
	Args: cobra.MaximumNArgs(2),
	Example: `
# Short codes: Opus 4.7 xhigh (1M ctx) + Haiku 4.5 low (200k ctx)
rush models use o47x h45l

# Sonnet 4.6 high (200k ctx) + Haiku 4.5 low — cheaper than Opus, still smart
rush models use s46h h45l

# Max thinking on smart (1M ctx), fast on fast
rush models use o47xx h45l

# Z.AI stack
rush models use glm5_3 glm5_turbo

# Mixed: Opus xhigh (1M ctx) + Z.AI turbo
rush models use o47x glm5_turbo

# Long-form atom syntax still works
rush models use opus-high sonnet-low

# Also set the worker slot (cheap sub-agent model) in the same call
rush models use o47x h45l --worker glm5_turbo

# Also set the reviewer slot (strongest model, --role reviewer only)
rush models use o47x h45l --reviewer oxx

# Set effort on a role slot in the same call: "<atom>-<level>" or "provider/model@level"
rush models use o47x h45l --reviewer glm5_3-max

# Set worker and reviewer together with smart/fast
rush models use o47x h45l --worker fl --reviewer oxx

# Workspace-only override (writes ./.rush/rush.json, leaves global untouched).
rush models use --local o47x h45l

# Raw "provider/model[@level]" syntax for models not in the registry.
rush models use openai/gpt-5@high zai/glm-5-turbo

# Change ONLY the fast slot, leaving smart/worker/reviewer untouched
rush models use --fast glm4_7_flash

# Change ONLY the smart slot
rush models use --smart o47x

# --smart and --fast together, still without touching worker/reviewer
rush models use --smart o47x --fast h45l

# After running, verify with:
rush models state
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		smartFlag, _ := cmd.Flags().GetString("smart")
		fastFlag, _ := cmd.Flags().GetString("fast")
		workerArg, _ := cmd.Flags().GetString("worker")
		reviewerArg, _ := cmd.Flags().GetString("reviewer")

		// Exactly one of the two forms documented above may be used per
		// call: positional <smart> <fast>, or --smart/--fast flags. Never
		// both — a call with both would leave it ambiguous which value
		// actually wins, and silently preferring one over the other is
		// worse than just refusing.
		var smartArg, fastArg string
		switch len(args) {
		case 2:
			if smartFlag != "" || fastFlag != "" {
				return fmt.Errorf("cannot combine positional <smart> <fast> args with --smart/--fast flags — use one form or the other (see `rush models use --help`)")
			}
			smartArg, fastArg = args[0], args[1]
		case 0:
			smartArg, fastArg = smartFlag, fastFlag
		default:
			return fmt.Errorf("expected 0 positional args (use --smart/--fast to update individual slots) or exactly 2 (<smart> <fast>) — got %d", len(args))
		}

		if smartArg == "" && fastArg == "" && workerArg == "" && reviewerArg == "" {
			return fmt.Errorf("nothing to set — provide <smart> <fast> positionally, or at least one of --smart/--fast/--worker/--reviewer")
		}

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		resolve := func(modelPart string) (string, string, bool, error) {
			provider, modelID, known, rerr := a.ResolveModel(modelPart)
			return provider, modelID, known, rerr
		}

		// warnUnknownModel surfaces the findModels unverified-passthrough
		// case (see its doc comment in internal/app/provider.go): the
		// provider is real and configured, but the model id didn't match
		// anything in its cached catalog. The set still goes through —
		// cost/context-window just render as unknown until the catalog is
		// refreshed or the provider's own API accepts/rejects the id at
		// call time — but the operator needs to see this wasn't a verified
		// hit, in case it's actually a typo.
		warnUnknownModel := func(label string, sel config.SelectedModel) {
			cmd.PrintErrf(
				"warning: %s model %s/%s is not in %s's known model catalog (cost/context-window unknown) -- set anyway. If this is a typo, fix it; otherwise run `rush providers update %s` to refresh the catalog.\n",
				label, sel.Provider, sel.Model, sel.Provider, sel.Provider,
			)
		}

		// Pass 1: parse + validate EVERY provided argument before writing
		// anything. This must have zero side effects — no config writes —
		// so that a validation failure on a later field (e.g. --reviewer)
		// can never leave an earlier field (smart/fast/--worker) already
		// persisted. See CLAUDE.md task tracking / bug report: previously
		// smart and fast were written immediately after being parsed, so a
		// bad --reviewer value failed the command AFTER smart/fast (and
		// worker) were already durably written to disk — a silent partial
		// write masquerading as a no-op failure.
		var smartSel config.SelectedModel
		if smartArg != "" {
			var lerr error
			var known bool
			smartSel, known, lerr = parseAtomOrRaw(smartArg, resolve)
			if lerr != nil {
				return fmt.Errorf("smart: %w", lerr)
			}
			if !known {
				warnUnknownModel("smart", smartSel)
			}
		}

		var fastSel config.SelectedModel
		if fastArg != "" {
			var serr error
			var known bool
			fastSel, known, serr = parseAtomOrRaw(fastArg, resolve)
			if serr != nil {
				return fmt.Errorf("fast: %w", serr)
			}
			if !known {
				warnUnknownModel("fast", fastSel)
			}
		}

		var workerSel config.SelectedModel
		if workerArg != "" {
			var werr error
			var known bool
			workerSel, known, werr = parseAtomOrRaw(workerArg, resolve)
			if werr != nil {
				return fmt.Errorf("worker: %w", werr)
			}
			if !known {
				warnUnknownModel("worker", workerSel)
			}
		}

		var reviewerSel config.SelectedModel
		if reviewerArg != "" {
			var rerr error
			var known bool
			reviewerSel, known, rerr = parseAtomOrRaw(reviewerArg, resolve)
			if rerr != nil {
				return fmt.Errorf("reviewer: %w", rerr)
			}
			if !known {
				warnUnknownModel("reviewer", reviewerSel)
			}
		}

		// Pass 2: every provided argument validated successfully — now, and
		// only now, write. All slots are written in a single call to
		// UpdatePreferredModels, which batches them into one SetConfigFields
		// write (one atomicWriteFile) instead of one write per slot — so
		// there's no window, even for an I/O-level failure, where only some
		// of the provided slots landed on disk. This reuses the same
		// batch-write primitive `rush providers patch` already relies on
		// (config.ConfigStore.SetConfigFields), rather than inventing a new
		// mechanism.
		toWrite := map[config.SelectedModelType]config.SelectedModel{}
		if smartArg != "" {
			toWrite[config.SelectedModelTypeSmart] = smartSel
		}
		if fastArg != "" {
			toWrite[config.SelectedModelTypeFast] = fastSel
		}
		if workerArg != "" {
			toWrite[config.SelectedModelTypeWorker] = workerSel
		}
		if reviewerArg != "" {
			toWrite[config.SelectedModelTypeReviewer] = reviewerSel
		}

		if err := a.Store().UpdatePreferredModels(scope, toWrite); err != nil {
			return fmt.Errorf("write models: %w", err)
		}

		if smartArg != "" {
			fmt.Fprintf(os.Stderr, "set smart = %s/%s%s in %s scope\n",
				smartSel.Provider, smartSel.Model, effortSuffix(smartSel.ReasoningEffort), scope)
		}
		if fastArg != "" {
			fmt.Fprintf(os.Stderr, "set fast = %s/%s%s in %s scope\n",
				fastSel.Provider, fastSel.Model, effortSuffix(fastSel.ReasoningEffort), scope)
		}
		if workerArg != "" {
			fmt.Fprintf(os.Stderr, "set worker = %s/%s%s in %s scope\n",
				workerSel.Provider, workerSel.Model, effortSuffix(workerSel.ReasoningEffort), scope)
		}
		if reviewerArg != "" {
			fmt.Fprintf(os.Stderr, "set reviewer = %s/%s%s in %s scope\n",
				reviewerSel.Provider, reviewerSel.Model, effortSuffix(reviewerSel.ReasoningEffort), scope)
		}

		return nil
	},
}

func effortSuffix(effort string) string {
	if effort == "" {
		return ""
	}
	return " effort=" + effort
}

func init() {
	modelsUseCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsUseCmd.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	modelsUseCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsUseCmd.Flags().String("smart", "", "Set only the smart slot (atom or provider/model[@level]) — cannot combine with positional args")
	modelsUseCmd.Flags().String("fast", "", "Set only the fast slot (atom or provider/model[@level]) — cannot combine with positional args")
	modelsUseCmd.Flags().String("worker", "", "Also set the optional worker slot (atom or provider/model[@level])")
	modelsUseCmd.Flags().String("reviewer", "", "Also set the optional reviewer slot (atom or provider/model[@level])")
	modelsCmd.AddCommand(modelsUseCmd)
}
