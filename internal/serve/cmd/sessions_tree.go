package cmd

import (
	"os"

	sessview "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/view"
	sessscan "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/view/scan"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

var sessionsTreeCmd = sessview.NewSessionsTreeCmd(setupApp)

type treeJSON = sessview.TreeJSON

func printTreeNode(w *os.File, s session.Session, children map[string][]session.Session, prefix string, isLast bool, maxDepth, currentDepth int) {
	sessview.PrintTreeNode(w, s, children, prefix, isLast, maxDepth, currentDepth)
}

func extractFilePaths(toolName, input string) []string {
	return sessscan.ExtractFilePaths(toolName, input)
}
