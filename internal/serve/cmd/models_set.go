// Fork patch: batch 11 — `rush models set` removed in favour of
// `rush models use <smart> <fast>`. This file keeps only:
//  1. A hidden cobra command that prints a redirect notice + exits 2.
//  2. `splitModelEffort`, the @level-suffix helper still used by atom parsing
//     and a couple of tests.
package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var modelsSetCmd = &cobra.Command{
	Use:                "set",
	Args:               cobra.NoArgs,
	Hidden:             true,
	Short:              "(removed — use `rush models use`)",
	DisableFlagParsing: true, // print the redirect even when caller passes --large/--small/etc.
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr,
			"`rush models set` was removed in batch 11.\n"+
				"Use `rush models use <smart> <fast>` instead (add --worker/--reviewer to set\n"+
				"those optional slots too). See `rush models list` for atoms.")
		exitWithAudit(2)
	},
}

// splitModelEffort splits "openai/gpt-5@high" into ("openai/gpt-5", "high").
// If no "@", returns (s, ""). The @ form is a CLI-only convenience so the
// user can pin reasoning effort in the same flag value.
func splitModelEffort(s string) (model, effort string) {
	at := -1
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return s, ""
	}
	return s[:at], s[at+1:]
}

// splitEffortTier splits the part after "@" ("high/fast") into the effort and the
// speed tier ("fast"); "/fast" alone sets only the tier.
func splitEffortTier(s string) (effort, tier string) {
	effort, tier, _ = strings.Cut(s, "/")
	return effort, tier
}

// effortWithTier is the inverse of splitEffortTier, for display and restore specs.
func effortWithTier(effort, tier string) string {
	if tier == "" {
		return effort
	}
	return effort + "/" + tier
}

// validateServiceTier accepts "" or "fast" on openai-codex models; other providers have no tier.
func validateServiceTier(provider, tier string) error {
	switch {
	case tier == "":
		return nil
	case tier != "fast":
		return fmt.Errorf("unknown service tier %q (valid: fast)", tier)
	case provider != "openai-codex":
		return fmt.Errorf("service tier %q is only supported on openai-codex models, not %s", tier, provider)
	}
	return nil
}
