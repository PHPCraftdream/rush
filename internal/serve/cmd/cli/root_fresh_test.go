package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRootFreshTreePerConstruction(t *testing.T) {
	root1 := NewRootCmd()
	root2 := NewRootCmd()

	assert.NotSame(t, root1, root2)

	commands := func(root *cobra.Command) map[string]*cobra.Command {
		byPath := make(map[string]*cobra.Command)
		var walk func(string, *cobra.Command)
		walk = func(prefix string, command *cobra.Command) {
			for _, child := range command.Commands() {
				path := strings.TrimSpace(prefix + " " + child.Name())
				byPath[path] = child
				walk(path, child)
			}
		}
		walk("", root)
		return byPath
	}

	commands1, commands2 := commands(root1), commands(root2)
	var sharedPaths []string
	for path, command := range commands1 {
		if commands2[path] == command {
			sharedPaths = append(sharedPaths, path)
		}
	}
	sort.Strings(sharedPaths)
	assert.Empty(t, sharedPaths, "commands shared between roots: %s", strings.Join(sharedPaths, ", "))

	flags := func(root *cobra.Command) map[*pflag.Flag]string {
		byFlag := make(map[*pflag.Flag]string)
		var walk func(string, *cobra.Command)
		walk = func(prefix string, command *cobra.Command) {
			path := strings.TrimSpace(prefix + " " + command.Name())
			command.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
				byFlag[flag] = path
			})
			for _, child := range command.Commands() {
				walk(path, child)
			}
		}
		walk("", root)
		return byFlag
	}

	flags1, flags2 := flags(root1), flags(root2)
	var sharedFlags []string
	for flag, path := range flags1 {
		if otherPath, ok := flags2[flag]; ok {
			sharedFlags = append(sharedFlags, flag.Name+" ("+path+"; "+otherPath+")")
		}
	}
	sort.Strings(sharedFlags)
	assert.Empty(t, sharedFlags, "flags shared between roots: %s", strings.Join(sharedFlags, ", "))

	host1, host2 := root1.Flags().Lookup("host"), root2.Flags().Lookup("host")
	require.NotNil(t, host1)
	require.NotNil(t, host2)
	assert.NotSame(t, host1, host2)
}
