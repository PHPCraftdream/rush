package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

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
		a, err := setupAppLite(cmd)
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
				dash(p.Name),
				dash(string(p.Type)),
				status,
				modelCount,
				maskKey(p.APIKey),
				dash(p.BaseURL),
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
		a, err := setupAppLite(cmd)
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
		fmt.Fprintf(os.Stdout, "name:        %s\n", dash(item.Name))
		fmt.Fprintf(os.Stdout, "type:        %s\n", dash(item.Type))
		fmt.Fprintf(os.Stdout, "status:      %s\n", status)
		fmt.Fprintf(os.Stdout, "api_key:     %s (present: %t)\n", item.APIKey, item.APIKeyPresent)
		fmt.Fprintf(os.Stdout, "base_url:    %s\n", dash(item.BaseURL))
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

		a, err := setupAppLite(cmd)
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

		fmt.Fprintf(os.Stdout, "provider:  %s (%s)\n", id, dash(string(p.Type)))
		fmt.Fprintf(os.Stdout, "endpoint:  %s\n", dash(p.BaseURL))
		fmt.Fprintf(os.Stdout, "models:    %d returned by server\n\n", len(models))

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tCONTEXT\tNAME")
		for _, m := range models {
			ctx := "—"
			if m.ContextWindow > 0 {
				ctx = fmt.Sprintf("%d", m.ContextWindow)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", m.ID, ctx, dash(m.Name))
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
		a, err := setupAppLite(cmd)
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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, dash(p.Name), dash(string(p.Type)), status, modelCount, maskKey(p.APIKey), dash(p.BaseURL))
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
		APIKey:        maskKey(p.APIKey),
		APIKeyPresent: p.APIKey != "",
		BaseURL:       p.BaseURL,
		Disabled:      p.Disable,
		Models:        len(p.Models),
		HasOAuth:      p.OAuthToken != nil,
	}
}
