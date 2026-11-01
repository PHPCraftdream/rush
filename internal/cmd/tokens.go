package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var (
	tokensJSON                                  bool
	tokensName, tokensPermission, tokensExpires string
	tokensScopes                                []string
)

var tokenIDPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

func validateTokenID(id string) error {
	if !tokenIDPattern.MatchString(id) {
		return tokenUsageError(fmt.Errorf("token ID must be exactly 8 lowercase hexadecimal characters"))
	}
	return nil
}

var tokensCreateCmd = &cobra.Command{
	Use:     "create",
	Short:   "Create an API bearer token",
	Long:    "Create a user-global API token. The secret is printed once. An unbound run token accesses every workspace session; this is not an execution or filesystem sandbox.\n\nExit codes: 0 success, 1 store or I/O error, 2 usage or validation error.",
	Example: "rush tokens create --name ci --scope run --expires 30d",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		expires, err := parseTokenExpiry(tokensExpires, time.Now())
		if err != nil {
			return tokenUsageError(fmt.Errorf("invalid expiry"))
		}
		scopes := tokensScopes
		if len(scopes) == 0 {
			scopes = []string{"run"}
		}
		if strings.TrimSpace(tokensName) == "" {
			return tokenUsageError(fmt.Errorf("token name is required"))
		}
		seen := map[string]bool{}
		for _, scope := range scopes {
			if scope != "read" && scope != "run" && scope != "config" && scope != "admin" {
				return tokenUsageError(fmt.Errorf("invalid scope (allowed: read, run, config, admin)"))
			}
			seen[scope] = true
		}
		permission := tokensPermission
		if permission == "" {
			permission = "ask"
		}
		if permission != "ask" && permission != "readonly" && permission != "auto" {
			return tokenUsageError(fmt.Errorf("invalid permission (allowed: ask, readonly, auto)"))
		}
		if permission == "auto" && !seen["admin"] {
			return tokenUsageError(fmt.Errorf("auto permission requires admin scope"))
		}
		plain, record, err := config.GlobalAPITokenStore().CreateAPIToken(tokensName, scopes, permission, expires)
		if err != nil {
			return tokenStoreError(err)
		}
		if tokensJSON {
			fmt.Fprintln(cmd.ErrOrStderr(), "API token created; secret is printed once in JSON output")
			data, err := json.Marshal(struct {
				Token  string          `json:"token"`
				Record config.APIToken `json:"record"`
			}{plain, record})
			if err != nil {
				return tokenStoreError(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
		} else {
			fmt.Fprintln(cmd.ErrOrStderr(), "API token created; secret is printed once")
			fmt.Fprintln(cmd.OutOrStdout(), plain)
		}
		return nil
	},
}

var tokensListCmd = &cobra.Command{
	Use: "list", Short: "List API tokens",
	Long:    "List API token metadata without secrets or credential digests.\n\nExit codes: 0 success, 1 store or I/O error, 2 usage or validation error.",
	Example: "rush tokens list --json", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		records, err := config.GlobalAPITokenStore().ListAPITokens()
		if err != nil {
			return tokenStoreError(err)
		}
		if tokensJSON {
			b, e := json.Marshal(records)
			if e != nil {
				return tokenStoreError(e)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}
		for _, r := range records {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", r.ID, r.Name, strings.Join(r.Scopes, ","))
		}
		return nil
	},
}

var tokensShowCmd = &cobra.Command{
	Use: "show ID", Short: "Show API token metadata",
	Long:    "Show a token record by id; the secret and credential digest are never available after creation.\n\nExit codes: 0 success, 1 store or I/O error, 2 usage or validation error, 4 token not found.",
	Example: "rush tokens show 12abcdef", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateTokenID(args[0]); err != nil {
			return err
		}
		r, err := config.GlobalAPITokenStore().ShowAPIToken(args[0])
		if err != nil {
			return tokenStoreError(err)
		}
		if tokensJSON {
			b, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				return tokenStoreError(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "ID: %s\nName: %s\nScopes: %s\nPermission: %s\nCreated: %s\n", r.ID, r.Name, strings.Join(r.Scopes, ","), r.Permission, r.Created.Format(time.RFC3339))
			if r.Expires != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Expires: %s\n", r.Expires.Format(time.RFC3339))
			}
			if r.RevokedAt != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Revoked: %s\n", r.RevokedAt.Format(time.RFC3339))
			}
		}
		return nil
	},
}

var tokensRevokeCmd = &cobra.Command{
	Use: "revoke ID", Short: "Revoke an API token",
	Long:    "Revoke an API token immediately; verification observes revocation on its next disk read. Revoking an already-revoked token succeeds.\n\nExit codes: 0 success, 1 store or I/O error, 2 usage or validation error, 4 token not found.",
	Example: "rush tokens revoke 12abcdef", Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		if err := validateTokenID(args[0]); err != nil {
			return err
		}
		return tokenStoreError(config.GlobalAPITokenStore().RevokeAPIToken(args[0]))
	},
}

var tokensCmd = &cobra.Command{
	Use: "tokens", Short: "Manage API bearer tokens",
	Long:    "Create, inspect, list and revoke user-global API tokens.\n\nExit codes: 0 success, 1 store or I/O error, 2 usage or validation error, 4 token not found.",
	Example: "rush tokens list", Args: cobra.NoArgs,
}

func init() {
	tokensCreateCmd.Flags().StringVar(&tokensName, "name", "", "Human-readable token name")
	tokensCreateCmd.Flags().StringSliceVar(&tokensScopes, "scope", []string{"run"}, "Repeated scopes: read, run, config, admin")
	tokensCreateCmd.Flags().StringVar(&tokensPermission, "permission", "ask", "Permission mode: ask, readonly, auto")
	tokensCreateCmd.Flags().StringVar(&tokensExpires, "expires", "", "Expiry: duration (30d) or RFC3339 timestamp")
	tokensCreateCmd.Flags().BoolVar(&tokensJSON, "json", false, "Output JSON")
	tokensListCmd.Flags().BoolVar(&tokensJSON, "json", false, "Output JSON")
	tokensShowCmd.Flags().BoolVar(&tokensJSON, "json", false, "Output JSON")
	tokensCmd.AddCommand(tokensCreateCmd, tokensListCmd, tokensShowCmd, tokensRevokeCmd)
}

func parseTokenExpiry(value string, now time.Time) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		if !t.After(now) {
			return nil, fmt.Errorf("expiry must be in the future")
		}
		return &t, nil
	}
	if strings.HasSuffix(value, "d") {
		n, err := time.ParseDuration(strings.TrimSuffix(value, "d") + "h")
		if err != nil {
			return nil, fmt.Errorf("invalid expiry")
		}
		n *= 24
		expiry := now.Add(n)
		if !expiry.After(now) {
			return nil, fmt.Errorf("expiry must be in the future")
		}
		return &expiry, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return nil, fmt.Errorf("invalid expiry")
	}
	expiry := now.Add(d)
	if !expiry.After(now) {
		return nil, fmt.Errorf("expiry must be in the future")
	}
	return &expiry, nil
}

func tokenUsageError(err error) error { return &tokenCLIError{code: 2, err: err} }
