package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func readAPICLIDoc(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "api", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// cliDocSection returns the text of "### rush api <path>" up to the next
// heading, and whether the heading exists.
func cliDocSection(doc, path string) (string, bool) {
	heading := "### rush api " + path + "\n"
	start := strings.Index(doc, heading)
	if start < 0 {
		return "", false
	}
	rest := doc[start+len(heading):]
	if end := strings.Index(rest, "\n#"); end >= 0 {
		rest = rest[:end]
	}
	return rest, true
}

func walkAPICommands(command *cobra.Command, path string, visit func(child *cobra.Command, childPath string)) {
	for _, child := range command.Commands() {
		if child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		childPath := strings.TrimSpace(path + " " + child.Name())
		visit(child, childPath)
		walkAPICommands(child, childPath, visit)
	}
}

// TestAPIDocsLint enforces the documentation contract of the command group:
// every command has help metadata and its own section in docs/api/cli.md,
// every flag has a usage string and is documented where it applies.
func TestAPIDocsLint(t *testing.T) {
	doc := readAPICLIDoc(t)
	root := newAPICmd()

	index := strings.Index(doc, "## Global flags")
	if index < 0 {
		t.Fatal("docs/api/cli.md has no \"## Global flags\" section")
	}
	globalSection := doc[index:]
	if end := strings.Index(globalSection[len("## Global flags"):], "\n## "); end >= 0 {
		globalSection = globalSection[:len("## Global flags")+end]
	}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Usage == "" {
			t.Errorf("global flag --%s has an empty usage string", f.Name)
		}
		if !strings.Contains(globalSection, "`--"+f.Name+"`") {
			t.Errorf("global flag --%s is not documented under \"## Global flags\"", f.Name)
		}
	})

	walkAPICommands(root, "", func(child *cobra.Command, path string) {
		if child.Short == "" || child.Long == "" || child.Example == "" {
			t.Errorf("rush api %s lacks Short, Long or Example", path)
		}
		if child.Runnable() && !strings.Contains(child.Long, "Exit codes:") {
			t.Errorf("runnable rush api %s lacks an \"Exit codes:\" block", path)
		}
		section, found := cliDocSection(doc, path)
		if !found {
			t.Errorf("docs/api/cli.md has no section \"### rush api %s\"", path)
		}
		child.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if f.Usage == "" {
				t.Errorf("rush api %s: flag --%s has an empty usage string", path, f.Name)
			}
			if found && !strings.Contains(section, "`--"+f.Name+"`") {
				t.Errorf("rush api %s: flag --%s is not documented in its section", path, f.Name)
			}
		})
	})
}

// TestAPIDocsDoNotDocumentMissingCommands is the reverse direction: a section
// for a command that no longer exists fails.
func TestAPIDocsDoNotDocumentMissingCommands(t *testing.T) {
	doc := readAPICLIDoc(t)
	known := map[string]bool{}
	walkAPICommands(newAPICmd(), "", func(_ *cobra.Command, path string) { known[path] = true })
	for _, line := range strings.Split(doc, "\n") {
		if path, ok := strings.CutPrefix(line, "### rush api "); ok && !known[strings.TrimSpace(path)] {
			t.Errorf("docs/api/cli.md documents a command that does not exist: %q", path)
		}
	}
}
