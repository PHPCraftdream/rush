package cmd

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// commandDiscoveryScope computes the provider ids whose live model-catalog
// discovery the invoked command actually needs. It returns nil when the
// command is not scoped (today's behavior: discover every enabled
// provider) or when any named argument cannot be resolved to a provider
// with certainty — scoping must then not be applied at all.
func commandDiscoveryScope(cmd *cobra.Command) []string {
	switch cmd.CommandPath() {
	case "rush models use":
		positional := cmd.Flags().Args()
		smart, fast := "", ""
		switch len(positional) {
		case 2:
			smart, fast = positional[0], positional[1]
		case 0:
			smart, _ = cmd.Flags().GetString("smart")
			fast, _ = cmd.Flags().GetString("fast")
		}
		worker, _ := cmd.Flags().GetString("worker")
		reviewer, _ := cmd.Flags().GetString("reviewer")
		return scopedDiscoveryProviders(smart, fast, worker, reviewer)
	default:
		return nil
	}
}

// scopedDiscoveryProviders resolves each argument to its provider id via
// the atom registry (atoms and short codes) or the raw provider/model
// split. Any unresolvable argument disables scoping (nil). An empty
// result also disables it: nothing was named, so nothing can be scoped.
func scopedDiscoveryProviders(values ...string) []string {
	var ids []string
	for _, v := range values {
		if v == "" {
			continue
		}
		id, ok := discoveryProviderForModelArg(v)
		if !ok {
			return nil
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// discoveryProviderForModelArg resolves one `models use` argument to its
// provider id: raw "provider/model[@level]" yields the prefix; anything
// else goes through the atom registry, which resolves short codes too.
func discoveryProviderForModelArg(name string) (string, bool) {
	if i := strings.Index(name, "/"); i >= 0 {
		if i == 0 {
			return "", false
		}
		return name[:i], true
	}
	sm, err := parseAtom(name)
	if err != nil {
		return "", false
	}
	return sm.Provider, true
}
