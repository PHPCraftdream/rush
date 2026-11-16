package cmd

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2/tree"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/discover"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List all available models from known providers",
	Long: `List all available models from known providers. Shows provider name and model IDs. Unconfigured providers are marked with (not configured).

Rush resolves models through four named slots (` + "`config.SelectedModelType`" + `):

  smart     the strong default slot; the top-level agent runs on it.
  fast      the cheap slot for trivial work.
  worker    optional. A cheap slot for delegated hands-on sub-task work. Never
            auto-selected as a top-level model. When configured and the run
            uses --role smart, sub-agents spawned by the agent tool run on it.
  reviewer  optional. The strongest slot, for explicit review invocations.
            Never auto-selected anywhere — reachable only via --role reviewer.

worker and reviewer are both optional: with neither configured, everything
behaves exactly as if only smart/fast existed. See ` + "`rush models use --help`" + `
to set any slot — including worker/reviewer via the ` + "`--worker`" + `/` + "`--reviewer`" + `
flags, with effort settable in that same call — and ` + "`rush models state --help`" + `
to see what's effective. ` + "`rush models unset --help`" + ` clears slots, worker/reviewer
included.

Any slot can pin a reasoning-effort level (low/medium/high/xhigh/max, though
what a level actually does — and whether it does anything at all — is
provider-specific). See ` + "`rush models efforts --help`" + ` for the two syntaxes
that set it, the per-provider semantics (e.g. Z.AI collapses low/medium/high
into one wire value), and per-model command examples.

Superseded model families are hidden from this list and the WebUI picker:
openai-codex shows gpt-6 and newer (gpt-5.x, o3 and codex-mini are hidden),
and glm models below 5.3 are hidden for every provider (glm-5.3 and newer
are shown). Unknown or unparseable model ids are never hidden, and a model
explicitly configured in rush.json keeps working even while hidden.

Authenticated model catalogs are cached globally for seven days. Run
` + "`rush models cache clear [openai-codex|stepfun|zai|all]`" + ` to clear them
and restart the WebUI to refresh a running server's in-memory list.`,
	Example: `# List all available models
rush models

# Search models
rush models gpt5`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, _, err := initCommandConfig(cmd)
		if err != nil {
			return err
		}

		term := strings.ToLower(strings.Join(args, " "))
		entries := collectListModelEntries(cfg.Config(), cfg.KnownProviders(), term)

		var providerIDs []string
		for id := range entries {
			providerIDs = append(providerIDs, id)
		}
		sort.Strings(providerIDs)

		if len(providerIDs) == 0 && len(args) == 0 {
			return fmt.Errorf("no providers found")
		}
		if len(providerIDs) == 0 {
			return fmt.Errorf("no providers found matching %q", term)
		}

		if !isatty.IsTerminal(os.Stdout.Fd()) {
			for _, providerID := range providerIDs {
				entry := entries[providerID]
				for _, modelID := range entry.models {
					fmt.Println(providerID + "/" + modelID)
				}
			}
			return nil
		}

		t := tree.New()
		for _, providerID := range providerIDs {
			entry := entries[providerID]
			label := providerID
			if !entry.configured {
				label += " (not configured)"
			}
			providerNode := tree.Root(label)
			for _, modelID := range entry.models {
				providerNode.Child(modelID)
			}
			t.Child(providerNode)
		}

		cmd.Println(t)
		return nil
	},
}

// modelsProviderEntry is one provider's row in the `rush models` output.
type modelsProviderEntry struct {
	name       string
	models     []string
	configured bool
}

// collectListModelEntries builds the provider-to-model-ids listing for
// `rush models`: configured providers first, then catwalk-known ones, with
// the superseded model families (discover.ModelVisible) hidden from both.
func collectListModelEntries(cfg *config.Config, knownProviders []catwalk.Provider, term string) map[string]*modelsProviderEntry {
	entries := make(map[string]*modelsProviderEntry)

	// Add configured providers first.
	for providerID, provider := range cfg.Providers.Seq2() {
		if provider.Disable {
			continue
		}
		entry := &modelsProviderEntry{
			name:       provider.Name,
			configured: true,
		}
		for _, model := range provider.Models {
			if !discover.ModelVisible(providerID, model.ID) {
				continue
			}
			if term != "" {
				matched := false
				for _, s := range []string{provider.ID, provider.Name, model.ID, model.Name} {
					if strings.Contains(strings.ToLower(s), term) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			entry.models = append(entry.models, model.ID)
		}
		if len(entry.models) > 0 {
			slices.Sort(entry.models)
			entries[providerID] = entry
		}
	}

	// Add known but unconfigured providers from catwalk.
	for _, kp := range knownProviders {
		providerID := string(kp.ID)
		if _, exists := entries[providerID]; exists {
			continue
		}
		entry := &modelsProviderEntry{
			name:       kp.Name,
			configured: false,
		}
		for _, model := range kp.Models {
			if !discover.ModelVisible(providerID, model.ID) {
				continue
			}
			if term != "" {
				matched := false
				for _, s := range []string{providerID, kp.Name, model.ID, model.Name} {
					if strings.Contains(strings.ToLower(s), term) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			entry.models = append(entry.models, model.ID)
		}
		if len(entry.models) > 0 {
			slices.Sort(entry.models)
			entries[providerID] = entry
		}
	}

	return entries
}

// NewModelsCmd returns a fresh rush models command tree on every call, with no
// shared command objects or flag sets. The package-var template commands remain
// available for white tests.
func NewModelsCmd() *cobra.Command {
	modelsTemplateInit()

	parent := modelsFreshCopy(modelsCmd)
	bump := modelsFreshCopy(modelsBumpCmd)
	cache := modelsFreshCopy(modelsCacheCmd)
	cacheClear := modelsFreshCopy(modelsCacheClearCmd)
	efforts := modelsFreshCopy(modelsEffortsCmd)
	list := modelsFreshCopy(modelsListCmd)
	preset := modelsFreshCopy(modelsPresetCmd)
	set := modelsFreshCopy(modelsSetCmd)
	state := modelsFreshCopy(modelsStateCmd)
	unset := modelsFreshCopy(modelsUnsetCmd)
	use := modelsFreshCopy(modelsUseCmd)

	bump.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	bump.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	bump.MarkFlagsMutuallyExclusive("global", "local")

	cache.AddCommand(cacheClear)

	list.Flags().Bool("json", false, "Emit a structured JSON object instead of human-readable text")
	list.Flags().Bool("refresh", false, "Force a network refresh of provider data before listing (default: read cache/embedded only, no network)")

	state.Flags().Bool("json", false, "Emit a structured JSON object")

	unset.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	unset.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	unset.MarkFlagsMutuallyExclusive("global", "local")

	use.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	use.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	use.MarkFlagsMutuallyExclusive("global", "local")
	use.Flags().String("smart", "", "Set only the smart slot (atom or provider/model[@level]) — cannot combine with positional args")
	use.Flags().String("fast", "", "Set only the fast slot (atom or provider/model[@level]) — cannot combine with positional args")
	use.Flags().String("worker", "", "Also set the optional worker slot (atom or provider/model[@level])")
	use.Flags().String("reviewer", "", "Also set the optional reviewer slot (atom or provider/model[@level])")

	parent.AddCommand(bump)
	parent.AddCommand(cache)
	parent.AddCommand(efforts)
	parent.AddCommand(list)
	parent.AddCommand(preset)
	parent.AddCommand(set)
	parent.AddCommand(state)
	parent.AddCommand(unset)
	parent.AddCommand(use)
	return parent
}

// modelsTemplateReady guards lazy priming so package-var white tests keep their
// flags, while NewModelsCmd returns fresh objects.
var modelsTemplateReady bool

// modelsTemplateInit primes the package-var command templates once. Construction
// is documented single-threaded.
func modelsTemplateInit() {
	if modelsTemplateReady {
		return
	}

	modelsBumpCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsBumpCmd.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	modelsBumpCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsCmd.AddCommand(modelsBumpCmd)

	modelsCacheCmd.AddCommand(modelsCacheClearCmd)
	modelsCmd.AddCommand(modelsCacheCmd)

	modelsCmd.AddCommand(modelsEffortsCmd)

	modelsListCmd.Flags().Bool("json", false, "Emit a structured JSON object instead of human-readable text")
	modelsListCmd.Flags().Bool("refresh", false, "Force a network refresh of provider data before listing (default: read cache/embedded only, no network)")
	modelsCmd.AddCommand(modelsListCmd)

	modelsCmd.AddCommand(modelsPresetCmd)

	// `models show` flag registration still belongs here for backwards
	// compat — the command itself lives in models_show.go (was inlined
	// into the old models_set.go before batch 11; if absent, this is a
	// no-op).
	modelsCmd.AddCommand(modelsSetCmd)

	modelsStateCmd.Flags().Bool("json", false, "Emit a structured JSON object")
	modelsCmd.AddCommand(modelsStateCmd)

	modelsUnsetCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsUnsetCmd.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	modelsUnsetCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsCmd.AddCommand(modelsUnsetCmd)

	modelsUseCmd.Flags().Bool("global", false, "Target the global config (default when neither --global nor --local is given)")
	modelsUseCmd.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json)")
	modelsUseCmd.MarkFlagsMutuallyExclusive("global", "local")
	modelsUseCmd.Flags().String("smart", "", "Set only the smart slot (atom or provider/model[@level]) — cannot combine with positional args")
	modelsUseCmd.Flags().String("fast", "", "Set only the fast slot (atom or provider/model[@level]) — cannot combine with positional args")
	modelsUseCmd.Flags().String("worker", "", "Also set the optional worker slot (atom or provider/model[@level])")
	modelsUseCmd.Flags().String("reviewer", "", "Also set the optional reviewer slot (atom or provider/model[@level])")
	modelsCmd.AddCommand(modelsUseCmd)

	modelsTemplateReady = true
}

func modelsFreshCopy(t *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:                t.Use,
		Short:              t.Short,
		Long:               t.Long,
		Example:            t.Example,
		Args:               t.Args,
		RunE:               t.RunE,
		Run:                t.Run,
		ValidArgs:          append([]string(nil), t.ValidArgs...),
		Aliases:            append([]string(nil), t.Aliases...),
		Hidden:             t.Hidden,
		DisableFlagParsing: t.DisableFlagParsing,
	}
}
