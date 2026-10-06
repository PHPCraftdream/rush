// Fork patch: `rush models state` RESTORE block — copy-pasteable commands.
package cmd

import (
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
)

// restoreCommands builds one `rush models use --<role> <spec>` command per
// slot explicitly set on disk; roles in order, global before local.
func restoreCommands(global, local map[config.SelectedModelType]*config.SelectedModel) []string {
	cmds := []string{}
	for _, role := range []config.SelectedModelType{
		config.SelectedModelTypeSmart,
		config.SelectedModelTypeFast,
		config.SelectedModelTypeWorker,
		config.SelectedModelTypeReviewer,
	} {
		for _, sc := range []struct {
			name string
			at   map[config.SelectedModelType]*config.SelectedModel
		}{{"global", global}, {"local", local}} {
			m := sc.at[role]
			if m == nil {
				continue
			}
			c := "rush models use --" + role.String() + " " + shellQuoteSpec(restoreSpec(*m))
			if sc.name == "local" {
				c += " --local"
			}
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// restoreSpec renders m like printEffectiveLine, but an EffortSource atom with
// no stored effort goes raw (`models use <atom>` rejects it without a level).
func restoreSpec(m config.SelectedModel) string {
	k := lookupAtomForModel(m)
	a := atomRegistry[k]
	if k == "" || (a.EffortSource != nil && m.ReasoningEffort == "") {
		spec := m.Provider + "/" + m.Model
		if m.ReasoningEffort != "" {
			spec += "@" + m.ReasoningEffort
		}
		return spec
	}
	if m.ReasoningEffort != "" && staleEffortNote(m) == "" {
		return k + "-" + m.ReasoningEffort
	}
	return k
}

// shellQuoteSpec single-quotes spec for POSIX sh unless every character is
// safe unquoted.
func shellQuoteSpec(spec string) string {
	for i := 0; i < len(spec); i++ {
		c := spec[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("-_.@/:+", c) >= 0 {
			continue
		}
		return "'" + strings.ReplaceAll(spec, "'", `'\''`) + "'"
	}
	return spec
}
