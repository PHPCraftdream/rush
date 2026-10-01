package shell

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// waitCommands are the builtins a pure "wait and tick" turn runs: sleeping or
// printing without observing anything.
var waitCommands = map[string]struct{}{
	"sleep":  {},
	"echo":   {},
	"printf": {},
	"true":   {},
	":":      {},
}

// IsNoOpCommand reports whether command is a pure wait: every call is a
// literal invocation of sleep/echo/printf/true/:, with no redirects, pipes,
// substitutions or expansions; `;` and `&&` between them are allowed. A parse
// failure or anything else (a real command, `sleep $N`, `echo $(date)`,
// `echo x > f`, `a | b`, `a || b`) is not a no-op.
func IsNoOpCommand(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	ok := true
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node := node.(type) {
		case nil:
			return true // Walk yields nils between real nodes
		case *syntax.CallExpr:
			if len(node.Assigns) != 0 || !isWaitCall(node.Args) {
				ok = false
			}
			return false
		case *syntax.BinaryCmd:
			if node.Op != syntax.AndStmt {
				ok = false
			}
			return true
		case *syntax.File:
			return true
		case *syntax.Stmt:
			if len(node.Redirs) != 0 {
				ok = false
			}
			return true
		default:
			ok = false
			return false
		}
	})
	return ok
}

// isWaitCall reports whether args is a literal call of a wait command.
func isWaitCall(args []*syntax.Word) bool {
	if len(args) == 0 {
		return false
	}
	head, ok := literalWord(args[0].Parts)
	if !ok {
		return false
	}
	if _, ok := waitCommands[head]; !ok {
		return false
	}
	for _, arg := range args[1:] {
		if _, ok := literalWord(arg.Parts); !ok {
			return false
		}
	}
	return true
}
