package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// deepFreshCmd returns a fresh deep copy of src, including its commands and
// flag sets. Call it for each construction: template vars stay untouched for
// white tests. For flags bound to package variables, use deepFreshCmdRebind so
// skip and rebind can preserve those bindings on the fresh command tree.
func deepFreshCmd(src *cobra.Command) *cobra.Command {
	return deepFreshCmdRebind(src, nil, nil)
}

// deepFreshCmdRebind returns a fresh deep copy of src, then calls rebind once
// with the completed tree. Flags named in skip are omitted from the copy so
// rebind can register fresh flags with their package-variable bindings.
func deepFreshCmdRebind(src *cobra.Command, skip map[string]bool, rebind func(fresh *cobra.Command)) *cobra.Command {
	dst := &cobra.Command{
		Use:                src.Use,
		Short:              src.Short,
		Long:               src.Long,
		Example:            src.Example,
		Args:               src.Args,
		RunE:               src.RunE,
		Run:                src.Run,
		ValidArgs:          cloneFreshStrings(src.ValidArgs),
		Aliases:            cloneFreshStrings(src.Aliases),
		Hidden:             src.Hidden,
		DisableFlagParsing: src.DisableFlagParsing,
		SilenceUsage:       src.SilenceUsage,
		SilenceErrors:      src.SilenceErrors,
		Deprecated:         src.Deprecated,
		Version:            src.Version,
		FParseErrWhitelist: src.FParseErrWhitelist,
	}

	cloneFreshFlags(src.Flags(), dst.Flags(), skip)
	cloneFreshFlags(src.PersistentFlags(), dst.PersistentFlags(), skip)

	for _, child := range src.Commands() {
		dst.AddCommand(deepFreshCmdRebind(child, skip, nil))
	}

	if rebind != nil {
		rebind(dst)
	}
	return dst
}

func cloneFreshFlags(src, dst *pflag.FlagSet, skip map[string]bool) {
	src.VisitAll(func(f *pflag.Flag) {
		if skip[f.Name] {
			return
		}

		var register func()
		switch f.Value.Type() {
		case "bool":
			register = func() { dst.BoolP(f.Name, f.Shorthand, false, f.Usage) }
		case "string":
			register = func() { dst.StringP(f.Name, f.Shorthand, "", f.Usage) }
		case "int":
			register = func() { dst.IntP(f.Name, f.Shorthand, 0, f.Usage) }
		case "int64":
			register = func() { dst.Int64P(f.Name, f.Shorthand, 0, f.Usage) }
		case "float64":
			register = func() { dst.Float64P(f.Name, f.Shorthand, 0, f.Usage) }
		case "duration":
			register = func() { dst.DurationP(f.Name, f.Shorthand, 0, f.Usage) }
		case "stringSlice":
			register = func() { dst.StringSliceP(f.Name, f.Shorthand, nil, f.Usage) }
		case "stringArray":
			register = func() { dst.StringArrayP(f.Name, f.Shorthand, nil, f.Usage) }
		default:
			panic(fmt.Errorf("deepFreshCmd: unsupported flag type %s for %s", f.Value.Type(), f.Name))
		}
		register()
		nf := dst.Lookup(f.Name)

		_ = nf.Value.Set(f.Value.String())
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
