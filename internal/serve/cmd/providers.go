package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/app"
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

var providersSetCmd = &cobra.Command{
	Use:   "set <id>",
	Short: "Write provider fields to the chosen scope",
	Long: `Set one or more provider fields in --global (default) or --local
scope. Only the flags you pass are written — unset fields are left
untouched, so you can update just an API key without erasing the
base URL.

Pass --disabled=true to disable a provider without losing its
credentials; --disabled=false to re-enable.

--peak-hours sets the refusal window (HH:MM-HH:MM, local time);
--peak-hours-message sets the custom text shown when a request is
refused inside that window. A time-only --peak-hours update preserves
the existing message; --peak-hours-message "" clears the message and
keeps the window; --peak-hours off clears both. The message requires
a window — set --peak-hours first.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Set api key in global config (default scope)
rush providers set openai --api-key=$OPENAI_API_KEY

# Pin a custom base URL just for this workspace
rush providers set openai --local --base-url=http://localhost:11434/v1

# Disable a provider without removing it
rush providers set hyper --disabled=true

# Refuse to run during business hours (local time); clear with "off"
rush providers set openai --peak-hours 09:00-18:00

# Customise the peak-hours refusal text (workspace scope only)
rush providers set openai --local --peak-hours-message "Ping #ops-oncall before retrying"

# Set window and message together
rush providers set openai --peak-hours 09:00-18:00 --peak-hours-message "Low quota until 18:00"
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		id := args[0]
		updates := map[string]any{}
		if cmd.Flags().Changed("api-key") {
			v, _ := cmd.Flags().GetString("api-key")
			updates["providers."+escapeSJSONPathComponent(id)+".api_key"] = v
		}
		if cmd.Flags().Changed("base-url") {
			v, _ := cmd.Flags().GetString("base-url")
			updates["providers."+escapeSJSONPathComponent(id)+".base_url"] = v
		}
		if cmd.Flags().Changed("type") {
			v, _ := cmd.Flags().GetString("type")
			updates["providers."+escapeSJSONPathComponent(id)+".type"] = v
		}
		if cmd.Flags().Changed("name") {
			v, _ := cmd.Flags().GetString("name")
			updates["providers."+escapeSJSONPathComponent(id)+".name"] = v
		}
		if cmd.Flags().Changed("disabled") {
			v, _ := cmd.Flags().GetBool("disabled")
			updates["providers."+escapeSJSONPathComponent(id)+".disable"] = v
		}
		clearPeakHours := false
		var peakHoursWindow *config.PeakHoursWindow
		peakHoursChanged := cmd.Flags().Changed("peak-hours")
		if peakHoursChanged {
			v, _ := cmd.Flags().GetString("peak-hours")
			w, err := parsePeakHoursWindow(v)
			if err != nil {
				return fmt.Errorf("invalid --peak-hours: %w", err)
			}
			if w == nil {
				clearPeakHours = true
			} else {
				peakHoursWindow = w
			}
		}
		messageChanged := cmd.Flags().Changed("peak-hours-message")
		peakHoursMessage := ""
		if messageChanged {
			peakHoursMessage, _ = cmd.Flags().GetString("peak-hours-message")
		}
		if len(updates) == 0 && !peakHoursChanged && !messageChanged {
			return fmt.Errorf("no fields to set — pass at least one of --api-key/--base-url/--type/--name/--disabled/--peak-hours/--peak-hours-message")
		}

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		if peakHoursWindow != nil || (messageChanged && !clearPeakHours) {
			// The merged provider view this command already uses for
			// every other field (workspace overrides global).
			var existing *config.PeakHoursWindow
			existingMessage := ""
			if p, ok := a.Store().Config().Providers.Get(id); ok {
				existingMessage = p.PeakHours.EffectiveMessage()
				if p.PeakHours != nil && p.PeakHours.Legacy != nil {
					w := *p.PeakHours.Legacy
					existing = &w
				}
			}
			if peakHoursWindow != nil {
				// --peak-hours alone only carries HH:MM-HH:MM: preserve
				// the existing message; --peak-hours-message overrides it.
				if messageChanged {
					peakHoursWindow.Message = peakHoursMessage
				} else {
					peakHoursWindow.Message = existingMessage
				}
			} else {
				// Message-only update: rewrite the effective window with
				// the new message so the target scope keeps a complete,
				// valid peak_hours object, never a bare message fragment.
				if existing == nil {
					// The effective entry may be a weekly policy rather than
					// a legacy window; rewrite an owned copy of it with the
					// new message instead of refusing.
					var eff *config.ProviderConfig
					if p, ok := a.Store().Config().Providers.Get(id); ok {
						eff = &p
					}
					if eff == nil || eff.PeakHours == nil || eff.PeakHours.Policy == nil {
						return fmt.Errorf("provider %q has no peak-hours window — set one first with `rush providers set %s --peak-hours HH:MM-HH:MM`", id, id)
					}
					pol := *eff.PeakHours.Policy
					pol.Windows = append([]config.PeakHoursWindowSpec(nil), eff.PeakHours.Policy.Windows...)
					pol.Message = peakHoursMessage
					updates["providers."+escapeSJSONPathComponent(id)+".peak_hours"] = &config.ProviderPeakHours{Policy: &pol}
				} else {
					peakHoursWindow = existing
					peakHoursWindow.Message = peakHoursMessage
				}
			}
			// Legacy empty/equal windows mean "off" — clear the field
			// instead of writing a shell object (see NormalizeForWrite).
			// Skipped for policy-form rewrites, which were already stored.
			if peakHoursWindow != nil {
				if (peakHoursWindow.Start == "" && peakHoursWindow.End == "") || peakHoursWindow.Start == peakHoursWindow.End {
					clearPeakHours = true
				} else {
					current, _ := a.Store().Config().Providers.Get(id)
					updates["providers."+escapeSJSONPathComponent(id)+".peak_hours"] = peakEntryWithLegacyWindow(current.PeakHours, peakHoursWindow)
				}
			}
		}

		if clearPeakHours {
			fieldKey := "providers." + escapeSJSONPathComponent(id) + ".peak_hours"
			if scope == config.ScopeWorkspace {
				// Explicit off persists a null override so it suppresses a
				// global policy instead of falling back to inheritance.
				if err := a.Store().SetConfigField(scope, fieldKey, nil); err != nil {
					return fmt.Errorf("failed to clear peak_hours: %w", err)
				}
				fmt.Fprintf(os.Stderr, "set peak_hours to explicit off (null) for provider %q in %s scope\n", id, scope)
			} else {
				if err := a.Store().RemoveConfigField(scope, fieldKey); err != nil {
					return fmt.Errorf("failed to clear peak_hours: %w", err)
				}
				fmt.Fprintf(os.Stderr, "cleared peak_hours for provider %q in %s scope\n", id, scope)
			}
		}
		if len(updates) > 0 {
			if err := a.Store().SetConfigFields(scope, updates); err != nil {
				return fmt.Errorf("failed to write provider config: %w", err)
			}
			fmt.Fprintf(os.Stderr, "wrote %d field(s) to %s scope for provider %q\n", len(updates), scope, id)
		}
		return nil
	},
}

var providersAddCmd = &cobra.Command{
	Use:   "add <id>",
	Short: "Add a new provider",
	Long: `Add a new provider to the chosen scope (default: global). Specify
the provider type, name, and optionally a base URL, API key,
peak-hours window, and peak-hours refusal message.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Add a catwalk-known provider (Z.AI)
rush providers add zai --name "Z.AI" --type openai-compat --base-url https://api.z.ai --api-key $ZAI_API_KEY

# Add a custom OpenAI-compatible provider
rush providers add local-llm --name "Local LLM" --type openai-compat --base-url http://localhost:8000/v1 --api-key none

# Add with a peak-hours window and custom refusal message
rush providers add zai --name "Z.AI" --type openai-compat --peak-hours 09:00-18:00 --peak-hours-message "Low quota until 18:00"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		name, _ := cmd.Flags().GetString("name")
		typeStr, _ := cmd.Flags().GetString("type")
		baseURL, _ := cmd.Flags().GetString("base-url")
		apiKey, _ := cmd.Flags().GetString("api-key")
		enable, _ := cmd.Flags().GetBool("enable")

		var peakHours *config.PeakHoursWindow
		if cmd.Flags().Changed("peak-hours") {
			phRaw, _ := cmd.Flags().GetString("peak-hours")
			ph, err := parsePeakHoursWindow(phRaw)
			if err != nil {
				return fmt.Errorf("invalid --peak-hours: %w", err)
			}
			peakHours = ph
		}

		peakHoursMessage := ""
		messageSet := cmd.Flags().Changed("peak-hours-message")
		if messageSet {
			peakHoursMessage, _ = cmd.Flags().GetString("peak-hours-message")
			if peakHours == nil {
				// A refusal message is only shown inside a window; absent
				// --peak-hours (or "off") means there is none.
				return fmt.Errorf("--peak-hours-message requires --peak-hours HH:MM-HH:MM")
			}
			peakHours.Message = peakHoursMessage
		}

		if name == "" {
			return fmt.Errorf("--name is required")
		}
		if typeStr == "" {
			return fmt.Errorf("--type is required")
		}

		// Check if provider already exists
		if _, exists := a.Config().Providers.Get(id); exists {
			return fmt.Errorf("provider %q already exists", id)
		}

		provType := catwalk.Type(typeStr)

		if provType == "cli" {
			return fmt.Errorf("adding new CLI providers requires editing internal/agent/provider/cliprovider/provider.go directly")
		}

		// Validate provider type
		validTypes := catwalk.KnownProviderTypes()
		validTypes = append(validTypes, "openai-compat")
		isValid := false
		for _, t := range validTypes {
			if t == provType {
				isValid = true
				break
			}
		}
		if !isValid {
			fmt.Fprintf(os.Stderr, "Supported provider types: %v\n", validTypes)
			return fmt.Errorf("unsupported provider type %q", typeStr)
		}

		// If base-url not provided for catwalk-known, try to get from catwalk
		if baseURL == "" {
			for _, known := range a.Store().KnownProviders() {
				if known.ID == catwalk.InferenceProvider(id) {
					baseURL = known.APIEndpoint
					break
				}
			}
		}

		pc := config.ProviderConfig{
			ID:      id,
			Name:    name,
			Type:    provType,
			BaseURL: baseURL,
			APIKey:  apiKey,
			Disable: !enable,
		}

		fields := map[string]any{
			"providers." + escapeSJSONPathComponent(id) + ".name":    name,
			"providers." + escapeSJSONPathComponent(id) + ".type":    typeStr,
			"providers." + escapeSJSONPathComponent(id) + ".disable": pc.Disable,
		}
		if baseURL != "" {
			fields["providers."+escapeSJSONPathComponent(id)+".base_url"] = baseURL
		}
		if apiKey != "" {
			fields["providers."+escapeSJSONPathComponent(id)+".api_key"] = apiKey
		}
		if peakHours != nil {
			fields["providers."+escapeSJSONPathComponent(id)+".peak_hours"] = peakHours
		}

		if err := a.Store().SetConfigFields(scope, fields); err != nil {
			return fmt.Errorf("failed to add provider: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ %s created\n", id)

		// Test connection if API key provided
		if apiKey != "" && !strings.HasPrefix(apiKey, "$") {
			if err := pc.TestConnection(a.Store().Resolver()); err != nil {
				fmt.Fprintf(os.Stderr, "✗ connection failed: %v (but provider saved; fix and re-enable)\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "✓ connection verified\n")
			}
		}
		// Fetch models if enabled
		if enable {
			if err := updateSingleProvider(a, id, scope); err != nil {
				return fmt.Errorf("provider created, but model refresh failed: %w", err)
			}
		}

		return nil
	},
}

var providersUpdateCmd = &cobra.Command{
	Use:   "update [<id> | --all]",
	Short: "Refresh provider models from the API",
	Long: `Fetch the latest model list from a provider's API and update the local
configuration. Shows a summary of added/removed models.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		all, _ := cmd.Flags().GetBool("all")

		if all && len(args) > 0 {
			return fmt.Errorf("--all and <id> are mutually exclusive")
		}

		if !all && len(args) == 0 {
			return fmt.Errorf("either specify <id> or use --all")
		}

		if all {
			return updateAllProviders(a)
		}

		return updateSingleProvider(a, args[0], config.ScopeGlobal)
	},
}

func updateSingleProvider(a *app.App, id string, scopes ...config.Scope) error {
	writeScope := config.ScopeGlobal
	if len(scopes) > 0 {
		writeScope = scopes[0]
	}
	p, ok := a.Config().Providers.Get(id)
	if !ok {
		return fmt.Errorf("provider %q not found", id)
	}
	return updateProvider(a, id, p, writeScope)
}

// updateProvider refreshes the model list of p (stored under id), a provider
// config the caller already holds. updateAllProviders passes the config it
// listed up front: an earlier provider's write reloads the live config, and
// that reload drops a provider with no models whose discovery fails at that
// moment, so looking the id up again would report "not found" instead of the
// provider's real fetch error.
func updateProvider(a *app.App, id string, p config.ProviderConfig, writeScope config.Scope) error {
	oldModels := p.Models
	newModels, warnings, err := fetchModels(a, p)
	if err != nil {
		return err
	}

	added, removed := computeDiff(oldModels, newModels)

	oldCount := len(oldModels)
	newCount := len(newModels)

	var diffStr strings.Builder
	if newCount > oldCount {
		fmt.Fprintf(&diffStr, " (+%d", newCount-oldCount)
		if len(added) > 0 && len(added) <= 3 {
			diffStr.WriteString(": ")
			for i, m := range added {
				if i > 0 {
					diffStr.WriteString(", ")
				}
				diffStr.WriteString(m.ID)
			}
		}
		diffStr.WriteString(")")
	} else if newCount < oldCount {
		fmt.Fprintf(&diffStr, " (-%d", oldCount-newCount)
		if len(removed) > 0 && len(removed) <= 3 {
			diffStr.WriteString(": ")
			for i, m := range removed {
				if i > 0 {
					diffStr.WriteString(", ")
				}
				diffStr.WriteString(m.ID)
			}
		}
		diffStr.WriteString(")")
	}

	fmt.Fprintf(os.Stderr, "%s: %d → %d models%s\n", id, oldCount, newCount, diffStr.String())

	// Check for orphaned preferred slots
	cfg := a.Config()
	for modelType, model := range cfg.Models {
		if model.Provider != id {
			continue
		}
		for _, rm := range removed {
			if rm.ID == model.Model {
				slotName := "smart"
				if modelType == config.SelectedModelTypeFast {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "WARN: preferred %s = %s/%s no longer exists after update — your '%s' slot is broken. Run `rush models use <smart> <fast>` to fix.\n", slotName, id, model.Model, slotName)
			}
		}
	}

	// Save updated models to config
	modelsJSON, _ := json.Marshal(newModels)
	updates := map[string]any{
		"providers." + escapeSJSONPathComponent(id) + ".models": json.RawMessage(modelsJSON),
	}
	if err := a.Store().SetConfigFields(writeScope, updates); err != nil {
		return fmt.Errorf("failed to save updated models: %w", err)
	}
	if supportsConfirmedProviderRefresh(p.Type) {
		refreshConfirmedProviderModels(a, id, p)
	}
	// Print warnings
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "note: %s\n", w)
	}

	return nil
}

func updateAllProviders(a *app.App) error {
	providers := a.Config().EnabledProviders()
	count := 0
	var updateErrors []error
	for _, p := range providers {
		if err := updateProvider(a, p.ID, p, config.ScopeGlobal); err != nil {
			fmt.Fprintf(os.Stderr, "error updating %s: %v\n", p.ID, err)
			updateErrors = append(updateErrors, fmt.Errorf("%s: %w", p.ID, err))
			continue
		}
		count++
	}
	fmt.Fprintf(os.Stderr, "Updated %d provider(s)\n", count)
	if len(updateErrors) > 0 {
		return fmt.Errorf("failed to update %d provider(s): %w", len(updateErrors), errors.Join(updateErrors...))
	}
	return nil
}

var providersEnableCmd = &cobra.Command{
	Use:   "enable <id>",
	Short: "Enable a provider and refresh its model list",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found, see `rush providers list`", id)
		}

		if !p.Disable {
			fmt.Fprintf(os.Stderr, "provider %q is already enabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "providers."+escapeSJSONPathComponent(id)+".disable", false); err != nil {
			return fmt.Errorf("failed to enable provider: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ %s enabled\n", id)
		return nil
	},
}

var providersDisableCmd = &cobra.Command{
	Use:   "disable <id>",
	Short: "Disable a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]
		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not found, see `rush providers list`", id)
		}

		if p.Disable {
			fmt.Fprintf(os.Stderr, "provider %q is already disabled\n", id)
			return nil
		}

		if err := a.Store().SetConfigField(scope, "providers."+escapeSJSONPathComponent(id)+".disable", true); err != nil {
			return fmt.Errorf("failed to disable provider: %w", err)
		}

		// Check if preferred slots use this provider
		cfg := a.Config()
		for modelType, model := range cfg.Models {
			if model.Provider == id {
				slotName := "smart"
				if modelType == config.SelectedModelTypeFast {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "warning: %s slot was using %s/%s; that slot is now broken. Run `rush models use <smart> <fast>` to fix.\n", slotName, id, model.Model)
			}
		}

		fmt.Fprintf(os.Stderr, "%s disabled\n", id)
		return nil
	},
}

var providersUnsetCmd = &cobra.Command{
	Use:     "unset <id>",
	Aliases: []string{"remove", "rm"},
	Short:   "Remove a provider entry from the chosen scope",
	Long: `Delete the providers.<id> object from the targeted config file. The
provider may still appear in "providers list" if it is also defined in
the other scope (workspace fallback to global, or vice versa) — run
unset with the matching --global / --local to fully clear it. For a
default/catwalk-known provider (e.g. "anthropic", "openai") unset only
clears any override; the provider reappears from the built-in catalog on
the next config load — use "rush providers disable <id>" to
persistently suppress it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		id := args[0]

		// Check if preferred slots use this provider and warn
		cfg := a.Config()
		for modelType, model := range cfg.Models {
			if model.Provider == id {
				slotName := "smart"
				if modelType == config.SelectedModelTypeFast {
					slotName = "fast"
				}
				fmt.Fprintf(os.Stderr, "warning: %s slot was using %s/%s; that slot is now broken. Run `rush models use <smart> <fast>` to fix.\n", slotName, id, model.Model)
			}
		}

		if err := a.Store().RemoveConfigField(scope, "providers."+escapeSJSONPathComponent(id)); err != nil {
			return fmt.Errorf("failed to remove provider from %s scope: %w", scope, err)
		}
		fmt.Fprintf(os.Stderr, "removed provider %q from %s scope\n", id, scope)
		return nil
	},
}
