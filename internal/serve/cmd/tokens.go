package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	tokensJSON                                                    bool
	tokensName, tokensPermission, tokensExpires, tokensPinSession string
	tokensScopes, tokensBatchTools                                []string
	tokensBatch                                                   bool
	tokensMaxRoots, tokensMaxParallel                             int
)

var batchToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// tokenBatchBinding builds the batch binding of `tokens create --batch`.
// Tool names are exact: a wildcard or any other punctuation is a usage error.
func tokenBatchBinding(tools []string, maxRoots, maxParallel int) (config.APITokenBinding, error) {
	if maxRoots < 0 || maxParallel < 0 {
		return config.APITokenBinding{}, fmt.Errorf("--max-roots and --max-parallel must not be negative")
	}
	seenTool := map[string]bool{}
	clean := make([]string, 0, len(tools))
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if !batchToolNamePattern.MatchString(tool) {
			return config.APITokenBinding{}, fmt.Errorf("invalid --tools entry %q: exact tool names only, no wildcards", tool)
		}
		if !seenTool[tool] {
			seenTool[tool] = true
			clean = append(clean, tool)
		}
	}
	return config.APITokenBinding{Kind: "batch", Batch: &config.APITokenBatchBinding{Tools: clean, MaxRoots: maxRoots, MaxParallel: maxParallel}}, nil
}

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
	Long:    "Create a user-global API token. The secret is printed once. An unbound run token accesses every workspace session; this is not an execution or filesystem sandbox. A binding (--pin-session or --batch) confines the API surface of the token, not the agent: if the permission mode lets the agent run a shell, a bound token is not isolated from the workspace, the session database or other sessions. A batch binding admits batch intake only (POST /tasks, POST /batches and their reads, stops and answers); it cannot be combined with the admin scope, so it cannot use the auto permission mode.\n\nExit codes: 0 success, 1 store or I/O error, 2 usage or validation error.",
	Example: "rush tokens create --name ci --scope run --expires 30d\nrush tokens create --name worker --batch --max-roots 4",
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
		if tokensBatch && tokensPinSession != "" {
			return tokenUsageError(fmt.Errorf("--batch cannot be combined with --pin-session"))
		}
		binding := config.APITokenBinding{Kind: "none"}
		if tokensPinSession != "" {
			cwd, err := cmd.InheritedFlags().GetString("cwd")
			if err != nil || cwd == "" {
				return tokenUsageError(fmt.Errorf("--pin-session requires --cwd"))
			}
			workspace, err := config.CanonicalAPIWorkspace(cwd)
			if err != nil {
				return tokenUsageError(fmt.Errorf("invalid --cwd: %w", err))
			}
			sessionID := tokensPinSession
			create := sessionID == "new"
			if create {
				sessionID = uuid.NewString()
			}
			binding = config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: sessionID, Workspace: workspace, Create: create}}
		}
		switch {
		case tokensBatch:
			if seen["admin"] {
				return tokenUsageError(fmt.Errorf("--batch cannot be combined with the admin scope"))
			}
			batchBinding, err := tokenBatchBinding(tokensBatchTools, tokensMaxRoots, tokensMaxParallel)
			if err != nil {
				return tokenUsageError(err)
			}
			binding = batchBinding
		case len(tokensBatchTools) > 0 || tokensMaxRoots != 0 || tokensMaxParallel != 0:
			return tokenUsageError(fmt.Errorf("--tools, --max-roots and --max-parallel require --batch"))
		}
		plain, record, err := config.GlobalAPITokenStore().CreateAPITokenWithBinding(tokensName, scopes, permission, expires, binding)
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
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", r.ID, r.Name, strings.Join(r.Scopes, ","), tokenBindingPresentation(r.Binding))
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
			fmt.Fprintf(cmd.OutOrStdout(), "ID: %s\nName: %s\nScopes: %s\nPermission: %s\nBinding: %s\nCreated: %s\n", r.ID, r.Name, strings.Join(r.Scopes, ","), r.Permission, tokenBindingPresentation(r.Binding), r.Created.Format(time.RFC3339))
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
	tokensCreateCmd.Flags().StringVar(&tokensPinSession, "pin-session", "", "Bind to one session ID, or new to generate one (requires --cwd)")
	tokensCreateCmd.Flags().BoolVar(&tokensBatch, "batch", false, "Bind to batch intake: the token submits tasks and batches only; session runs, creation and forks are refused (excludes --pin-session and admin scope)")
	tokensCreateCmd.Flags().StringSliceVar(&tokensBatchTools, "tools", nil, "With --batch: extra exact tool names the token may submit besides agent (repeatable, no wildcards)")
	tokensCreateCmd.Flags().IntVar(&tokensMaxRoots, "max-roots", 0, "With --batch: live batch roots at once (0 = server default)")
	tokensCreateCmd.Flags().IntVar(&tokensMaxParallel, "max-parallel", 0, "With --batch: parallel leaves per batch (0 = server default)")
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
