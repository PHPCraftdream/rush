package cmd

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

type customRoleView struct {
	Name   string            `json:"name"`
	Source string            `json:"source"`
	Role   config.CustomRole `json:"role"`
}

func newRolesCmd() *cobra.Command {
	command := &cobra.Command{Use: "roles", Short: "Manage custom model roles and optional tool restrictions", Args: cobra.NoArgs}
	command.PersistentFlags().String("scope", "", "Configuration scope: global or local")
	command.PersistentFlags().Bool("global", false, "Use the global configuration")
	command.PersistentFlags().Bool("local", false, "Use the local configuration")
	command.PersistentFlags().Bool("json", false, "Print machine-readable JSON")
	command.AddCommand(
		&cobra.Command{Use: "list", Short: "List custom roles and their effective source", Args: cobra.NoArgs, RunE: runCustomRolesRead},
		&cobra.Command{Use: "show <name>", Short: "Show a custom role", Args: cobra.ExactArgs(1), RunE: runCustomRolesRead},
		&cobra.Command{Use: "remove <name>", Short: "Remove a scoped role definition; lower scopes may become effective", Args: cobra.ExactArgs(1), RunE: runCustomRoleRemove},
	)
	set := &cobra.Command{
		Use: "set <name> <provider/model[@effort]>", Short: "Create or update a custom role", Args: cobra.ExactArgs(2), RunE: runCustomRoleSet,
		Long: "Create or update a role using the model atom or provider/model@effort syntax.\n" +
			"Omitting --tools preserves an existing scoped tools list; a new definition has no extra restriction.\n" +
			"Use --tools '[]' for no tools or --inherit-tools to remove the restriction explicitly.\n" +
			"Only * and ? are wildcard characters; permissions and ancestor restrictions still apply.",
	}
	set.Flags().String("tools", "", "Allowed tool-name patterns as a JSON array; [] denies all tools")
	set.Flags().Bool("inherit-tools", false, "Remove this role's tools field")
	set.MarkFlagsMutuallyExclusive("tools", "inherit-tools")
	command.AddCommand(set)
	return command
}

func customRoleCommandScope(cmd *cobra.Command) (config.Scope, error) {
	explicit, _ := cmd.Flags().GetString("scope")
	if cmd.Flags().Changed("scope") {
		if cmd.Flags().Changed("global") || cmd.Flags().Changed("local") {
			return config.ScopeGlobal, fmt.Errorf("--scope cannot be combined with --global or --local")
		}
		switch explicit {
		case "global":
			return config.ScopeGlobal, nil
		case "local", "workspace":
			return config.ScopeWorkspace, nil
		default:
			return config.ScopeGlobal, fmt.Errorf("invalid role scope %q: use global or local", explicit)
		}
	}
	return scopeFromFlags(cmd, config.ScopeGlobal)
}

func customRoleToolsInput(cmd *cobra.Command) (*[]string, error) {
	if !cmd.Flags().Changed("tools") {
		return nil, nil
	}
	raw, _ := cmd.Flags().GetString("tools")
	var patterns []string
	if err := json.Unmarshal([]byte(raw), &patterns); err != nil {
		return nil, fmt.Errorf("--tools requires a JSON array: %w", err)
	}
	if patterns == nil {
		return nil, fmt.Errorf("--tools requires an array, not null")
	}
	return &patterns, nil
}

func runCustomRoleSet(cmd *cobra.Command, args []string) error {
	scope, err := customRoleCommandScope(cmd)
	if err != nil {
		return err
	}
	if err := config.ValidateCustomRoleName(args[0]); err != nil {
		return err
	}
	patterns, err := customRoleToolsInput(cmd)
	if err != nil {
		return err
	}
	store, _, _, err := initCommandConfig(cmd)
	if err != nil {
		return err
	}
	cfg, _ := store.Snapshot()
	selected, _, err := parseAtomOrRaw(args[1], func(value string) (string, string, bool, error) {
		return app.ResolveConfiguredModel(cfg, value)
	})
	if err != nil {
		return err
	}
	if _, exists := cfg.Providers.Get(selected.Provider); !exists {
		return fmt.Errorf("provider %q is not configured", selected.Provider)
	}
	inherit, _ := cmd.Flags().GetBool("inherit-tools")
	if err := store.SetCustomRole(scope, args[0], config.CustomRoleUpdate{Model: selected, Tools: patterns, InheritTools: inherit}); err != nil {
		return err
	}
	return printCustomRoleAtScope(cmd, store, scope, args[0])
}

func runCustomRoleRemove(cmd *cobra.Command, args []string) error {
	scope, err := customRoleCommandScope(cmd)
	if err != nil {
		return err
	}
	if err := config.ValidateCustomRoleName(args[0]); err != nil {
		return err
	}
	store, _, _, err := initCommandConfig(cmd)
	if err != nil {
		return err
	}
	if err := store.RemoveCustomRole(scope, args[0]); err != nil {
		return err
	}
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"removed": args[0], "scope": scope.String(), "lower_scope_may_apply": true})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed %q from %s; lower-scope definitions may now apply.\n", args[0], scope)
	return err
}

func printCustomRoleAtScope(cmd *cobra.Command, store *config.ConfigStore, scope config.Scope, name string) error {
	roles, err := store.CustomRolesForScope(scope)
	if err != nil {
		return err
	}
	role, exists := roles[name]
	if !exists {
		return fmt.Errorf("custom role %q is not defined at %s scope", name, scope)
	}
	return printCustomRoleViews(cmd, []customRoleView{{Name: name, Source: scope.String(), Role: role}}, true)
}

func runCustomRolesRead(cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		if err := config.ValidateCustomRoleName(args[0]); err != nil {
			return err
		}
	}
	scope, err := customRoleCommandScope(cmd)
	if err != nil {
		return err
	}
	store, _, _, err := initCommandConfig(cmd)
	if err != nil {
		return err
	}
	explicit := cmd.Flags().Changed("scope") || cmd.Flags().Changed("global") || cmd.Flags().Changed("local")
	roles := store.Config().Roles
	local := map[string]config.CustomRole{}
	global := map[string]config.CustomRole{}
	if explicit {
		roles, err = store.CustomRolesForScope(scope)
	} else {
		global, err = store.CustomRolesForScope(config.ScopeGlobal)
		if err == nil && store.HasWorkspaceConfig() {
			local, err = store.CustomRolesForScope(config.ScopeWorkspace)
		}
	}
	if err != nil {
		return err
	}
	views := make([]customRoleView, 0, len(roles))
	for name, role := range roles {
		if len(args) != 0 && name != args[0] {
			continue
		}
		source := "inherited"
		switch {
		case explicit:
			source = scope.String()
		case local[name].Model != "" && reflect.DeepEqual(local[name], role):
			source = "local"
		case global[name].Model != "" && reflect.DeepEqual(global[name], role):
			source = "global"
		}
		views = append(views, customRoleView{Name: name, Source: source, Role: role})
	}
	if len(args) != 0 && len(views) == 0 {
		return fmt.Errorf("custom role %q is not configured", args[0])
	}
	slices.SortFunc(views, func(a, b customRoleView) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return printCustomRoleViews(cmd, views, len(args) != 0)
}

func printCustomRoleViews(cmd *cobra.Command, views []customRoleView, single bool) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		if single {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(views[0])
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(views)
	}
	for _, view := range views {
		tools := "unrestricted within existing permissions"
		if view.Role.Tools != nil {
			encoded, err := json.Marshal(*view.Role.Tools)
			if err != nil {
				return err
			}
			tools = string(encoded)
		}
		model := view.Role.Provider + "/" + view.Role.Model
		if view.Role.ReasoningEffort != "" {
			model += "@" + view.Role.ReasoningEffort
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", view.Name, view.Source, model, tools); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newRolesCmd())
}
