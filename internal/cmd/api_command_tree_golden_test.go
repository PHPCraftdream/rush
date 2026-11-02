package cmd

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var updateAPICommandGolden = flag.Bool("update-api-command-golden", false, "update the API command tree golden")

func TestAPICommandTreeGolden(t *testing.T) {
	var lines []string
	var walk func(prefix string, command *cobra.Command)
	walk = func(prefix string, command *cobra.Command) {
		for _, child := range command.Commands() {
			path := strings.TrimSpace(prefix + " " + child.Name())
			lines = append(lines, path+"|"+child.Use)
			child.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
				lines = append(lines, path+"|--"+f.Name+"|"+f.Shorthand+"|"+f.Value.Type()+"|"+f.DefValue)
			})
			walk(path, child)
		}
	}
	walk("api", newAPICmd())
	sort.Strings(lines)
	actual := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "api_command_tree.golden")
	if *updateAPICommandGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(actual), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != actual {
		t.Fatalf("API command tree differs from %s; update only with -update-api-command-golden", path)
	}
}
