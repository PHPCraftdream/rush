package cmd

import (
	"sync"

	"github.com/spf13/cobra"
)

// NewProvidersCmd returns the rush providersCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newProvidersCmdOnce sync.Once

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

		// Package-level vars are initialized before any init() runs, so
		// providersCmd exists by the time this registers the subcommands.
		peakHoursListCmd.Flags().Bool("json", false, "Emit one NDJSON object per provider instead of a table")
		peakHoursShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")

		for _, c := range []*cobra.Command{
			peakHoursListCmd, peakHoursShowCmd, peakHoursSetCmd, peakHoursAddCmd,
			peakHoursRmCmd, peakHoursTzCmd, peakHoursClearCmd, peakHoursPauseCmd,
			peakHoursResumeCmd,
		} {
			c.Flags().Bool("global", false, "Target the global config (~/.local/share/rush/rush.json). Default when neither --global nor --local is given.")
			c.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json).")
			c.MarkFlagsMutuallyExclusive("global", "local")
		}

		peakHoursSetCmd.Flags().String("tz", "", "Timezone for the windows: IANA name, UTC+08:00 offset, utc, or local (stores an omitted timezone)")
		peakHoursSetCmd.Flags().String("message", "", "Refusal text shown while the provider is blocked. Empty string clears it.")

		providersPeakHoursCmd.AddCommand(
			peakHoursListCmd, peakHoursShowCmd, peakHoursSetCmd, peakHoursAddCmd,
			peakHoursRmCmd, peakHoursTzCmd, peakHoursClearCmd, peakHoursPauseCmd,
			peakHoursResumeCmd,
		)
		providersCmd.AddCommand(providersPeakHoursCmd)

		providersTestCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")
		providersTestCmd.Flags().Duration("timeout", 0, "HTTP timeout (default 1m)")
		providersCmd.AddCommand(providersTestCmd)
	})
	return deepFreshCmd(providersCmd)
}
