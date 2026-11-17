package sessview

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsTreeCmdTemplate = &cobra.Command{
	Use:   "tree [<root-id>]",
	Short: "Show session hierarchy as an ASCII tree",
	Long: `Display sessions in a tree view showing parent-child relationships.

All sessions (including sub-agent children) are shown. Leaves show message
count and cost. Use --depth to limit recursion and --json for structured output.`,
	Args: cobra.MaximumNArgs(1),
	Example: `
# Show all sessions as a tree
rush sessions tree

# Show tree for a specific root session
rush sessions tree my-session-id

# JSON output for scripting
rush sessions tree --json
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		return sessionsTreeCmdRun(cmd, args, cmdkit.SetupApp)
	},
}

var sessionsTreeFlagsOnce sync.Once

func NewSessionsTreeCmd(setup func(*cobra.Command) (*app.App, error)) *cobra.Command {
	sessionsTreeFlagsOnce.Do(func() {
		sessionsTreeCmdTemplate.Flags().Int("depth", 0, "Max tree depth (0 = unlimited)")
		sessionsTreeCmdTemplate.Flags().Bool("json", false, "Emit nested JSON instead of ASCII tree")
	})
	cmd := cmdkit.DeepFreshCmd(sessionsTreeCmdTemplate)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return sessionsTreeCmdRun(cmd, args, setup)
	}
	return cmd
}

func sessionsTreeCmdRun(cmd *cobra.Command, args []string, setup func(*cobra.Command) (*app.App, error)) error {
	depth, _ := cmd.Flags().GetInt("depth")
	asJSON, _ := cmd.Flags().GetBool("json")

	a, err := setup(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()
	ctx := cmd.Context()

	sessions, err := a.Sessions.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Build children map.
	children := make(map[string][]session.Session)
	var roots []session.Session
	for _, s := range sessions {
		if s.ParentSessionID == "" {
			roots = append(roots, s)
		} else {
			children[s.ParentSessionID] = append(children[s.ParentSessionID], s)
		}
	}

	// If root-id given, filter roots.
	if len(args) > 0 {
		sess, err := cmdkit.ResolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}
		roots = []session.Session{sess}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, root := range roots {
			node := buildTreeNode(root, children, depth, 0)
			if err := enc.Encode(node); err != nil {
				return err
			}
		}
		return nil
	}

	for _, root := range roots {
		PrintTreeNode(os.Stdout, root, children, "", true, depth, 0)
	}
	return nil
}

type TreeJSON struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Msgs     int64      `json:"msgs"`
	Cost     float64    `json:"cost"`
	Children []TreeJSON `json:"children,omitempty"`
}

func buildTreeNode(s session.Session, children map[string][]session.Session, maxDepth, currentDepth int) TreeJSON {
	node := TreeJSON{
		ID:    s.ID,
		Title: s.Title,
		Msgs:  s.MessageCount,
		// #1130: per-node OWN spend; the parent never carries the child's.
		Cost: s.OwnCost,
	}
	if maxDepth > 0 && currentDepth >= maxDepth {
		return node
	}
	ch := children[s.ID]
	node.Children = make([]TreeJSON, len(ch))
	for i, c := range ch {
		node.Children[i] = buildTreeNode(c, children, maxDepth, currentDepth+1)
	}
	return node
}

func PrintTreeNode(w *os.File, s session.Session, children map[string][]session.Session, prefix string, isLast bool, maxDepth, currentDepth int) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}
	if currentDepth == 0 {
		connector = ""
	}

	title := cliout.Truncate(s.Title, 40)
	fmt.Fprintf(w, "%s%s%-40s (%d msgs, $%.2f)\n",
		prefix, connector, title, s.MessageCount, s.OwnCost)

	if maxDepth > 0 && currentDepth >= maxDepth {
		return
	}

	ch := children[s.ID]
	for i, c := range ch {
		last := i == len(ch)-1
		childPrefix := prefix
		if currentDepth > 0 {
			if isLast {
				childPrefix += "    "
			} else {
				childPrefix += "│   "
			}
		}
		PrintTreeNode(w, c, children, childPrefix, last, maxDepth, currentDepth+1)
	}
}
