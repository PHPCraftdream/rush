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
// substitutions or expansions; `;` and `&&` between them are allowed. Literal
// `cd <path> &&` prefixes are allowed too, as `cd` observes nothing. A parse
// failure or anything else (a real command, `sleep $N`, `echo $(date)`,
// `cd $D && sleep 1`, `cd x; sleep 1`, `echo x > f`, `a | b`, `a || b`) is not
// a no-op.
func IsNoOpCommand(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	for _, stmt := range file.Stmts {
		if !isWaitStmt(stmt, false) {
			return false
		}
	}
	return true
}

// isWaitStmt reports whether stmt is a wait-only statement. A stmt carrying
// prefix sits on the left side of `&&`, where a literal `cd <path>` is
// tolerated; anywhere else `cd` is a real command, not a wait.
func isWaitStmt(stmt *syntax.Stmt, prefix bool) bool {
	if len(stmt.Redirs) != 0 {
		return false
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if len(cmd.Assigns) != 0 {
			return false
		}
		if prefix && isCdCall(cmd) {
			return true
		}
		return isWaitCall(cmd.Args)
	case *syntax.BinaryCmd:
		if cmd.Op != syntax.AndStmt {
			return false
		}
		return isWaitStmt(cmd.X, true) && isWaitStmt(cmd.Y, prefix)
	default:
		return false
	}
}

// isCdCall reports whether call is a literal `cd <path>`: the head is `cd` and
// exactly one literal argument follows, so no options, no `cd -` and no
// substitutions.
func isCdCall(call *syntax.CallExpr) bool {
	if len(call.Args) != 2 {
		return false
	}
	head, ok := literalWord(call.Args[0].Parts)
	if !ok || head != "cd" {
		return false
	}
	path, ok := literalWord(call.Args[1].Parts)
	if !ok || path == "-" || path == "--" {
		return false
	}
	return true
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
