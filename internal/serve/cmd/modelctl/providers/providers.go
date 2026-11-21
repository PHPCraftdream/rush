package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	provpeak "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/providers/peakhours"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var providersCmd = &cobra.Command{
	Use:   "providers",
	Short: "Inspect and edit LLM-provider configuration",
	Long: `Manage the provider entries rush will use for chat completions.

Provider config lives under "providers.<id>" in rush.json. Two scopes
exist and rush merges them at load time, workspace overriding global:

  --global   ~/.local/share/rush/rush.json   (or %LocalAppData%\rush on Windows)
  --local    ./.rush/rush.json               (next to the project)

If --global / --local is omitted the default is --global for write
operations and "both" for read operations.

Each provider entry holds: api_key, base_url, type, name, disable, and
an optional peak_hours refusal window (local-clock HH:MM-HH:MM; see
'rush providers set --help').`,
}

var providersListCmd = &cobra.Command{
	Use:   "list",
	Args:  cobra.NoArgs,
	Short: "List configured providers across both scopes",
	Long: `Print the merged effective view of providers (workspace overriding
global). Use --json for one NDJSON object per provider. API keys are
always masked — only the last 4 chars are shown.`,
	Example: `
rush providers list
rush providers list --json | jq 'select(.api_key_present)'
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := cmdkit.SetupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		providers := a.Config().Providers.Copy()
		// Stable order.
		ids := make([]string, 0, len(providers))
		for id := range providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		grepPattern, _ := cmd.Flags().GetString("grep")
		grepLower := strings.ToLower(grepPattern)

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, id := range ids {
				p := providers[id]
				if grepPattern != "" {
					if !matchesGrep(id, p, grepLower) {
						continue
					}
				}
				if err := enc.Encode(makeProviderListItem(id, p)); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATUS\tMODELS\tAPI_KEY\tBASE_URL\tPEAK")
		for _, id := range ids {
			p := providers[id]
			if grepPattern != "" {
				if !matchesGrep(id, p, grepLower) {
					continue
				}
			}
			status := "enabled"
			if p.Disable {
				status = "disabled"
			}
			modelCount := "—"
			if len(p.Models) > 0 {
				modelCount = fmt.Sprintf("%d", len(p.Models))
			}
			peak := peakHoursSummary(p.PeakHours)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				id,
				cliout.Dash(p.Name),
				cliout.Dash(string(p.Type)),
				status,
				modelCount,
				cliout.MaskKey(p.APIKey),
				cliout.Dash(p.BaseURL),
				peak,
			)
		}
		return tw.Flush()
	},
}

var providersShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Print a provider's effective config (api_key masked)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := cmdkit.SetupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		p, ok := a.Config().Providers.Get(args[0])
		if !ok {
			return fmt.Errorf("provider %q not configured", args[0])
		}
		item := makeProviderListItem(args[0], p)
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(item)
		}
		status := "enabled"
		if item.Disabled {
			status = "disabled"
		}
		fmt.Fprintf(os.Stdout, "id:          %s\n", item.ID)
		fmt.Fprintf(os.Stdout, "name:        %s\n", cliout.Dash(item.Name))
		fmt.Fprintf(os.Stdout, "type:        %s\n", cliout.Dash(item.Type))
		fmt.Fprintf(os.Stdout, "status:      %s\n", status)
		fmt.Fprintf(os.Stdout, "api_key:     %s (present: %t)\n", item.APIKey, item.APIKeyPresent)
		fmt.Fprintf(os.Stdout, "base_url:    %s\n", cliout.Dash(item.BaseURL))
		fmt.Fprintf(os.Stdout, "models:      %d\n", item.Models)
		fmt.Fprintf(os.Stdout, "oauth:       %t\n", item.HasOAuth)
		if ph := p.PeakHours; ph != nil && ph.Legacy != nil {
			state := "not in peak"
			if ph.Legacy.InPeakHours(time.Now()) {
				state = "in peak"
			}
			fmt.Fprintf(os.Stdout, "peak hours:  %s-%s (currently: %s)\n", ph.Legacy.Start, ph.Legacy.End, state)
			// Show the configured refusal message (set via --peak-hours-message).
			if msg := ph.EffectiveMessage(); msg != "" {
				fmt.Fprintf(os.Stdout, "peak hours message: %s\n", msg)
			}
		} else if ph := p.PeakHours; ph != nil && ph.Policy != nil {
			// Weekly policy detail lives in 'rush providers peak-hours show'.
			fmt.Fprintf(os.Stdout, "peak hours:  %s\n", peakHoursSummary(ph))
		}
		return nil
	},
}

var providersFetchModelsCmd = &cobra.Command{
	Use:   "fetch-models <id>",
	Short: "Query the provider's API live and list available models (read-only)",
	Long: `Hit the provider's models endpoint (GET <base_url>/models for
openai-compat, GET /v1/models for anthropic) using the resolved API
key and print whatever the server actually returns. Unlike
'rush providers update', this does NOT write anything to rush.json —
use it to discover newly-released models before deciding whether to
update the local cache.

Superseded model families are hidden from the printed list, the same
filter the other model listings use: openai-codex shows gpt-6 and
newer, and glm models below 5.3 are hidden for every provider.`,
	Example: `
# Inspect z.ai's live model list (looks for newly-released GLM versions)
rush providers fetch-models zai

# JSON for scripts
rush providers fetch-models openai --json

# Diff against locally-cached models
rush providers fetch-models zai --diff
  `,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		showDiff, _ := cmd.Flags().GetBool("diff")

		a, err := cmdkit.SetupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found", id)
		}

		models, warnings, err := fetchModels(a, p)
		if err != nil {
			return fmt.Errorf("fetch-models %s: %w", id, err)
		}

		if asJSON {
			out := struct {
				Provider string          `json:"provider"`
				Count    int             `json:"count"`
				Models   []catwalk.Model `json:"models"`
				Warnings []string        `json:"warnings,omitempty"`
			}{Provider: id, Count: len(models), Models: models, Warnings: warnings}
			return json.NewEncoder(os.Stdout).Encode(out)
		}

		fmt.Fprintf(os.Stdout, "provider:  %s (%s)\n", id, cliout.Dash(string(p.Type)))
		fmt.Fprintf(os.Stdout, "endpoint:  %s\n", cliout.Dash(p.BaseURL))
		fmt.Fprintf(os.Stdout, "models:    %d returned by server\n\n", len(models))

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tCONTEXT\tNAME")
		for _, m := range models {
			ctx := "—"
			if m.ContextWindow > 0 {
				ctx = fmt.Sprintf("%d", m.ContextWindow)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", m.ID, ctx, cliout.Dash(m.Name))
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		if showDiff {
			added, removed := computeDiff(p.Models, models)
			fmt.Fprintln(os.Stdout)
			fmt.Fprintf(os.Stdout, "diff vs cached (%d locally):\n", len(p.Models))
			if len(added) == 0 && len(removed) == 0 {
				fmt.Fprintln(os.Stdout, "  no changes")
			}
			for _, m := range added {
				fmt.Fprintf(os.Stdout, "  + %s\n", m.ID)
			}
			for _, m := range removed {
				fmt.Fprintf(os.Stdout, "  - %s\n", m.ID)
			}
		}

		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "note: %s\n", w)
		}
		return nil
	},
}

var providersGrepCmd = &cobra.Command{
	Use:   "grep <pattern>",
	Short: "Filter providers by id, name, or type (sugar for `list --grep <pattern>`)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := cmdkit.SetupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		needle := strings.ToLower(args[0])
		providers := a.Config().Providers.Copy()
		ids := make([]string, 0, len(providers))
		for id := range providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID	NAME	TYPE	STATUS	MODELS	API_KEY	BASE_URL")
		matched := 0
		for _, id := range ids {
			p := providers[id]
			if !matchesGrep(id, p, needle) {
				continue
			}
			status := "enabled"
			if p.Disable {
				status = "disabled"
			}
			modelCount := "—"
			if len(p.Models) > 0 {
				modelCount = fmt.Sprintf("%d", len(p.Models))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, cliout.Dash(p.Name), cliout.Dash(string(p.Type)), status, modelCount, cliout.MaskKey(p.APIKey), cliout.Dash(p.BaseURL))
			matched++
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if matched == 0 {
			fmt.Fprintf(os.Stderr, "no providers matched %q\n", args[0])
		}
		return nil
	},
}

// providerListItem is the JSON shape of providers list / show. Kept
// separate from config.ProviderConfig so api_key is never serialised as
// plaintext by accident.
type providerListItem struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Type          string `json:"type,omitempty"`
	APIKey        string `json:"api_key"` // masked
	APIKeyPresent bool   `json:"api_key_present"`
	BaseURL       string `json:"base_url,omitempty"`
	Disabled      bool   `json:"disabled"`
	Models        int    `json:"models"`
	HasOAuth      bool   `json:"oauth"`
}

func makeProviderListItem(id string, p config.ProviderConfig) providerListItem {
	return providerListItem{
		ID:            id,
		Name:          p.Name,
		Type:          string(p.Type),
		APIKey:        cliout.MaskKey(p.APIKey),
		APIKeyPresent: p.APIKey != "",
		BaseURL:       p.BaseURL,
		Disabled:      p.Disable,
		Models:        len(p.Models),
		HasOAuth:      p.OAuthToken != nil,
	}
}

// peakHoursSummary renders the compact PEAK column of providers list
// for either stored form: legacy start-end, or the weekly window count
// with the timezone when one is configured.
func peakHoursSummary(ph *config.ProviderPeakHours) string {
	if ph == nil || (ph.Legacy == nil && ph.Policy == nil) {
		return "—"
	}
	if ph.Legacy != nil {
		peak := ph.Legacy.Start + "-" + ph.Legacy.End
		if ph.Legacy.InPeakHours(time.Now()) {
			peak += " *"
		}
		return peak
	}
	pol := ph.Policy
	peak := fmt.Sprintf("%d windows", len(pol.Windows))
	if pol.Timezone != "" {
		peak += " (" + pol.Timezone + ")"
	}
	if ph.InPeak(time.Now()) {
		peak += " *"
	}
	return peak
}

var newProvidersCmdOnce sync.Once

// NewProvidersCmd returns a fresh providers command tree.
func NewProvidersCmd() *cobra.Command {
	newProvidersCmdOnce.Do(func() {
		providersListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")
		providersListCmd.Flags().String("grep", "", "Filter providers by id, name, or type (case-insensitive substring match)")
		providersShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")

		for _, c := range []*cobra.Command{providersSetCmd, providersUnsetCmd} {
			c.Flags().Bool("global", false, "Target the global config (~/.local/share/rush/rush.json). Default when neither --global nor --local is given.")
			c.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json).")
			c.MarkFlagsMutuallyExclusive("global", "local")
		}
		providersSetCmd.Flags().String("api-key", "", "API key for the provider (literal or $VAR — the latter expands at config-load time)")
		providersSetCmd.Flags().String("base-url", "", "Base URL for the provider's API")
		providersSetCmd.Flags().String("type", "", "Provider type: openai|openai-compat|anthropic|gemini|azure|vertexai")
		providersSetCmd.Flags().String("name", "", "Human-readable display name shown in the WUI")
		providersSetCmd.Flags().Bool("disabled", false, "Mark provider as disabled (kept in config, ignored at runtime)")
		providersSetCmd.Flags().String("peak-hours", "", "Daily peak-hours window as HH:MM-HH:MM. Legacy entries use local time; weekly entries keep their timezone and pause. Pass 'off' or empty to clear; --local preserves explicit off instead of inheriting the global policy.")
		providersSetCmd.Flags().String("peak-hours-message", "", "Custom text shown when a request is refused during the peak-hours window. Empty string clears it. Requires an existing --peak-hours window.")

		for _, c := range []*cobra.Command{providersEnableCmd, providersDisableCmd, providersAddCmd} {
			c.Flags().Bool("global", false, "Target the global config (~/.local/share/rush/rush.json). Default when neither --global nor --local is given.")
			c.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json).")
			c.MarkFlagsMutuallyExclusive("global", "local")
		}

		providersAddCmd.Flags().String("name", "", "Human-readable name for the provider (required)")
		providersAddCmd.Flags().String("type", "", "Provider type: openai|openai-compat|anthropic|gemini|azure|vertexai|bedrock|xai|zai|groq|openrouter|synthetic|huggingface|copilot|vercel (required)")
		providersAddCmd.Flags().String("base-url", "", "Base URL for the provider's API (optional for catwalk-known providers)")
		providersAddCmd.Flags().String("api-key", "", "API key for the provider (optional, can be set later)")
		providersAddCmd.Flags().Bool("enable", true, "Enable the provider after creation (default: true)")
		providersAddCmd.Flags().String("peak-hours", "", "Optional peak-hours window as HH:MM-HH:MM (local time). Provider is refused during this window.")
		providersAddCmd.Flags().String("peak-hours-message", "", "Optional custom text shown when a request is refused during the peak-hours window. Requires --peak-hours.")

		providersUpdateCmd.Flags().Bool("all", false, "Update all enabled providers")

		providersFetchModelsCmd.Flags().Bool("json", false, "Emit a JSON object instead of a table")
		providersFetchModelsCmd.Flags().Bool("diff", false, "Also show added/removed vs the locally-cached model list")

		providersCmd.AddCommand(providersListCmd, providersShowCmd, providersSetCmd, providersAddCmd, providersEnableCmd, providersDisableCmd, providersUnsetCmd, providersUpdateCmd, providersGrepCmd, providersFetchModelsCmd)

		providersPatchCmd.Flags().String("json", "", "Literal JSON object to merge into providers.<id>")
		providersPatchCmd.Flags().String("json-file", "", "Path to a file containing the JSON object")
		providersPatchCmd.Flags().Bool("global", false, "Target the global config (default)")
		providersPatchCmd.Flags().Bool("local", false, "Target the workspace config")
		providersPatchCmd.MarkFlagsMutuallyExclusive("global", "local")
		providersPatchCmd.MarkFlagsMutuallyExclusive("json", "json-file")
		providersCmd.AddCommand(providersPatchCmd)

		providersCmd.AddCommand(provpeak.NewPeakHoursCmd())

		providersTestCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")
		providersTestCmd.Flags().Duration("timeout", 0, "HTTP timeout (default 1m)")
		providersCmd.AddCommand(providersTestCmd)
	})
	return cmdkit.DeepFreshCmd(providersCmd)
}
