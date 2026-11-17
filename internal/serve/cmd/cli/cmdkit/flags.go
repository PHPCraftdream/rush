package cmdkit

import (
	"fmt"
	"strconv"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func ScopeFromFlags(cmd *cobra.Command, def config.Scope) (config.Scope, error) {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	switch {
	case global && local:
		return def, fmt.Errorf("--global and --local are mutually exclusive")
	case global:
		return config.ScopeGlobal, nil
	case local:
		return config.ScopeWorkspace, nil
	default:
		return def, nil
	}
}

// cmdFlagString reads a string flag, returning "" when the command has
// no such flag — used for best-effort attribution (process start log
// record) on commands that may or may not define the flag.
func CmdFlagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// Deep command-tree copying (K-lane helper; constructors use it for fresh per-call trees).

// DeepFreshCmd returns a fresh deep copy of src, including its commands and
// flag sets. Call it for each construction: template vars stay untouched for
// white tests. For flags bound to package variables, use DeepFreshCmdRebind so
// skip and rebind can preserve those bindings on the fresh command tree.
func DeepFreshCmd(src *cobra.Command) *cobra.Command {
	return DeepFreshCmdRebind(src, nil, nil)
}

// DeepFreshCmdRebind returns a fresh deep copy of src, then calls rebind once
// with the completed tree. Flags named in skip are omitted from the copy so
// rebind can register fresh flags with their package-variable bindings.
func DeepFreshCmdRebind(src *cobra.Command, skip map[string]bool, rebind func(fresh *cobra.Command)) *cobra.Command {
	dst := &cobra.Command{
		Use:                        src.Use,
		Aliases:                    cloneFreshStrings(src.Aliases),
		SuggestFor:                 cloneFreshStrings(src.SuggestFor),
		Short:                      src.Short,
		GroupID:                    src.GroupID,
		Long:                       src.Long,
		Example:                    src.Example,
		ValidArgs:                  cloneFreshStrings(src.ValidArgs),
		ValidArgsFunction:          src.ValidArgsFunction,
		Args:                       src.Args,
		ArgAliases:                 cloneFreshStrings(src.ArgAliases),
		BashCompletionFunction:     src.BashCompletionFunction,
		Deprecated:                 src.Deprecated,
		Annotations:                cloneFreshStringMap(src.Annotations),
		Version:                    src.Version,
		PersistentPreRun:           src.PersistentPreRun,
		PersistentPreRunE:          src.PersistentPreRunE,
		PreRun:                     src.PreRun,
		PreRunE:                    src.PreRunE,
		Run:                        src.Run,
		RunE:                       src.RunE,
		PostRun:                    src.PostRun,
		PostRunE:                   src.PostRunE,
		PersistentPostRun:          src.PersistentPostRun,
		PersistentPostRunE:         src.PersistentPostRunE,
		FParseErrWhitelist:         src.FParseErrWhitelist,
		CompletionOptions:          src.CompletionOptions,
		TraverseChildren:           src.TraverseChildren,
		Hidden:                     src.Hidden,
		SilenceErrors:              src.SilenceErrors,
		SilenceUsage:               src.SilenceUsage,
		DisableFlagParsing:         src.DisableFlagParsing,
		DisableAutoGenTag:          src.DisableAutoGenTag,
		DisableFlagsInUseLine:      src.DisableFlagsInUseLine,
		DisableSuggestions:         src.DisableSuggestions,
		SuggestionsMinimumDistance: src.SuggestionsMinimumDistance,
	}
	for _, group := range src.Groups() {
		groupCopy := *group
		dst.AddGroup(&groupCopy)
	}

	cloneFreshFlags(src.Flags(), dst.Flags(), skip, src, dst)
	cloneFreshFlags(src.PersistentFlags(), dst.PersistentFlags(), skip, src, dst)

	for _, child := range src.Commands() {
		dst.AddCommand(DeepFreshCmdRebind(child, skip, nil))
	}

	if rebind != nil {
		rebind(dst)
	}
	return dst
}

func cloneFreshFlags(src, dst *pflag.FlagSet, skip map[string]bool, srcCmd, dstCmd *cobra.Command) {
	dst.SortFlags = src.SortFlags
	dst.ParseErrorsAllowlist = src.ParseErrorsAllowlist
	dst.ParseErrorsWhitelist = src.ParseErrorsWhitelist //nolint:staticcheck // pflag still reads the deprecated field while parsing
	dst.Usage = src.Usage
	dst.SetNormalizeFunc(src.GetNormalizeFunc())

	src.VisitAll(func(f *pflag.Flag) {
		if skip[f.Name] {
			return
		}

		var register func()
		switch f.Value.Type() {
		case "bool":
			value, err := strconv.ParseBool(f.DefValue)
			if err != nil {
				panic(fmt.Errorf("deepFreshCmd: invalid default for %s: %w", f.Name, err))
			}
			register = func() { dst.BoolP(f.Name, f.Shorthand, value, f.Usage) }
		case "string":
			register = func() { dst.StringP(f.Name, f.Shorthand, f.DefValue, f.Usage) }
		case "int":
			value, err := strconv.ParseInt(f.DefValue, 0, 64)
			if err != nil {
				panic(fmt.Errorf("deepFreshCmd: invalid default for %s: %w", f.Name, err))
			}
			register = func() { dst.IntP(f.Name, f.Shorthand, int(value), f.Usage) }
		case "int64":
			value, err := strconv.ParseInt(f.DefValue, 0, 64)
			if err != nil {
				panic(fmt.Errorf("deepFreshCmd: invalid default for %s: %w", f.Name, err))
			}
			register = func() { dst.Int64P(f.Name, f.Shorthand, value, f.Usage) }
		case "float64":
			value, err := strconv.ParseFloat(f.DefValue, 64)
			if err != nil {
				panic(fmt.Errorf("deepFreshCmd: invalid default for %s: %w", f.Name, err))
			}
			register = func() { dst.Float64P(f.Name, f.Shorthand, value, f.Usage) }
		case "duration":
			value, err := time.ParseDuration(f.DefValue)
			if err != nil {
				panic(fmt.Errorf("deepFreshCmd: invalid default for %s: %w", f.Name, err))
			}
			register = func() { dst.DurationP(f.Name, f.Shorthand, value, f.Usage) }
		case "stringSlice":
			value := cloneFreshStrings(f.Value.(pflag.SliceValue).GetSlice())
			register = func() { dst.StringSliceP(f.Name, f.Shorthand, value, f.Usage) }
		case "stringArray":
			value := cloneFreshStrings(f.Value.(pflag.SliceValue).GetSlice())
			register = func() { dst.StringArrayP(f.Name, f.Shorthand, value, f.Usage) }
		default:
			panic(fmt.Errorf("deepFreshCmd: unsupported flag type %s for %s", f.Value.Type(), f.Name))
		}
		register()
		nf := dst.Lookup(f.Name)
		nf.Changed = false
		nf.DefValue = f.DefValue
		nf.NoOptDefVal = f.NoOptDefVal
		nf.Hidden = f.Hidden
		nf.Deprecated = f.Deprecated
		nf.ShorthandDeprecated = f.ShorthandDeprecated
		if f.Annotations != nil {
			nf.Annotations = make(map[string][]string, len(f.Annotations))
			for key, values := range f.Annotations {
				nf.Annotations[key] = cloneFreshStrings(values)
			}
		}
		if completion, ok := srcCmd.GetFlagCompletionFunc(f.Name); ok {
			if err := dstCmd.RegisterFlagCompletionFunc(f.Name, completion); err != nil {
				panic(fmt.Errorf("deepFreshCmd: copy completion for %s: %w", f.Name, err))
			}
		}
	})
}

func cloneFreshStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneFreshStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
