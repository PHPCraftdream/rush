package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestRootCommandTreeGolden(t *testing.T) {
	var lines []string
	var missingArgs []string
	var walk func(prefix string, command *cobra.Command)
	walk = func(prefix string, command *cobra.Command) {
		for _, child := range command.Commands() {
			path := strings.TrimSpace(prefix + " " + child.Name())
			lines = append(lines, path+"|"+child.Use)
			child.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
				lines = append(lines, path+"|--"+f.Name+"|"+f.Shorthand+"|"+f.Value.Type()+"|"+f.DefValue)
			})
			if child.Runnable() && !child.HasSubCommands() && child.Args == nil {
				missingArgs = append(missingArgs, child.CommandPath())
			}
			walk(path, child)
		}
	}
	walk("", NewRootCmd())
	sort.Strings(lines)
	actual := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "root_command_tree.golden")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != actual {
		actualSet := make(map[string]struct{})
		for _, line := range strings.Split(strings.TrimSuffix(actual, "\n"), "\n") {
			actualSet[line] = struct{}{}
		}
		goldenSet := make(map[string]struct{})
		for _, line := range strings.Split(strings.TrimSuffix(string(want), "\n"), "\n") {
			goldenSet[line] = struct{}{}
		}
		var extra, missing []string
		for line := range actualSet {
			if _, ok := goldenSet[line]; !ok {
				extra = append(extra, "extra: "+line)
			}
		}
		for line := range goldenSet {
			if _, ok := actualSet[line]; !ok {
				missing = append(missing, "missing: "+line)
			}
		}
		sort.Strings(extra)
		sort.Strings(missing)
		diagnostics := append(extra, missing...)
		diagnostics = append(diagnostics, "count: extra="+strconv.Itoa(len(extra))+" missing="+strconv.Itoa(len(missing)))
		t.Errorf("root command tree differs from %s:\n%s", path, strings.Join(diagnostics, "\n"))
	}
	if len(missingArgs) > 0 {
		sort.Strings(missingArgs)
		t.Errorf("runnable leaf commands without Args: %s", strings.Join(missingArgs, ", "))
	}
}
