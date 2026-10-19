package agentguard

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const (
	opaqueWrapperDetail = "under RUSH_DENY_EXEC dynamic commands are refused because tests/builds run only in GitHub CI; use a literal allowed command"
	policyDynamicArg    = "\x00rush-dynamic-argument"
)

func (p CommandPolicy) checkOpaqueWrapper(args []string) (string, bool) {
	if len(p.rules) == 0 || len(args) == 0 {
		return "", false
	}
	head := policyCanonicalExecutable(args[0])
	payload, mode := "", ""
	switch head {
	case "sh", "bash", "zsh", "dash":
		payload = policyShellPayload(args)
		mode = "sh"
	case "powershell", "pwsh":
		for i := 1; i < len(args); i++ {
			switch strings.ToLower(args[i]) {
			case "-file":
				return "", false
			case "-encodedcommand", "-enc":
				return args[i], true
			case "-command", "-c":
				payload, mode = strings.Join(args[i+1:], " "), "ps"
				i = len(args)
			case "-executionpolicy", "-windowstyle":
				i++
			}
		}
	case "cmd", "cmd.exe":
		for i := 1; i < len(args); i++ {
			if strings.EqualFold(args[i], "/c") || strings.EqualFold(args[i], "/k") {
				payload, mode = strings.Join(args[i+1:], " "), "cmd"
				break
			}
		}
	}
	if payload == "" {
		return "", false
	}
	if mode == "cmd" && strings.Contains(payload, "%") {
		return payload, true
	}
	if mode == "ps" {
		return payload, opaquePowerShell(payload)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(payload), "")
	if err != nil {
		return payload, true
	}
	opaque, reason := false, payload
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		head, dynamic, index := resolvePolicyShellHead(call)
		if dynamic {
			if head == "" || p.deniedHead(head) {
				opaque, reason = true, payload
			}
			return true
		}
		head = policyUnescapeWord(head)
		if head == "eval" || head == "source" || head == "." {
			opaque, reason = true, head
			return true
		}
		if p.deniedHead(head) && dynamicAction(call, index) {
			opaque, reason = true, payload
		}
		return true
	})
	return reason, opaque
}

func (p CommandPolicy) deniedHead(head string) bool {
	switch head {
	case "go":
		return p.hasRule("go")
	case "npm", "pnpm", "yarn", "npx", "bun":
		return p.hasRule("js")
	case "node":
		return p.hasRule("node")
	case "python", "python3", "py":
		return p.hasRule("python")
	case "cargo":
		return p.hasRule("cargo")
	case "pytest", "playwright", "vitest", "jest":
		return p.hasRule("test-runners")
	case "golangci-lint", "staticcheck", "revive", "sqlc", "gopls", "gotestsum", "ginkgo":
		return p.hasRule("linters")
	}
	return false
}
func (p CommandPolicy) hasRule(name string) bool { _, ok := p.rules[name]; return ok }

// resolvePolicyShellHead returns the effective executable and its argv index.
func resolvePolicyShellHead(call *syntax.CallExpr) (string, bool, int) {
	args := make([]string, len(call.Args))
	for i, word := range call.Args {
		value, literal := policyLiteralWord(word.Parts)
		if literal {
			args[i] = value
		} else {
			args[i] = policyDynamicArg
		}
	}
	return resolveArgvPolicyHead(args)
}

func dynamicAction(call *syntax.CallExpr, headIndex int) bool {
	words := make([]string, len(call.Args))
	dyn := make([]bool, len(call.Args))
	for i, w := range call.Args {
		words[i], dyn[i] = policyLiteralWord(w.Parts)
		dyn[i] = !dyn[i]
	}
	head := policyCanonicalExecutable(words[headIndex])
	i := headIndex + 1
	for i < len(words) {
		if dyn[i] {
			return true
		}
		name := words[i]
		if !strings.HasPrefix(name, "-") {
			if head == "npm" || head == "pnpm" || head == "yarn" || head == "bun" || head == "npx" {
				if name == "run" || name == "run-script" {
					i++
					return i < len(words) && dyn[i]
				}
			}
			if head == "go" && name == "mod" {
				i++
				return i < len(words) && dyn[i]
			}
			return false
		}
		option := name
		if eq := strings.IndexByte(option, '='); eq >= 0 {
			option = option[:eq]
		}
		if commandPolicyOptionOperands[head][option] && !strings.Contains(name, "=") {
			i++
			if i < len(words) && dyn[i] {
				return true
			}
		}
		i++
	}
	return false
}
func isEnvAssignment(value string) bool { i := strings.IndexByte(value, '='); return i > 0 }
func policyUnescapeWord(value string) string {
	if strings.EqualFold(value, `\eval`) {
		return "eval"
	}
	if strings.EqualFold(value, `\source`) {
		return "source"
	}
	return value
}

func policyLiteralWord(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			v, ok := policyLiteralWord(p.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(v)
		default:
			return "", false
		}
	}
	return b.String(), true
}

func policyShellCalls(source string) ([][]string, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(source), "")
	if err != nil {
		return nil, true
	}
	var calls [][]string
	opaque := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		args := make([]string, len(call.Args))
		for i, w := range call.Args {
			v, literal := policyLiteralWord(w.Parts)
			if literal {
				args[i] = v
			} else {
				args[i] = policyDynamicArg
			}
		}
		calls = append(calls, args)
		return true
	})
	return calls, opaque
}

func containsPowerShellCommand(source, word string) bool {
	for start := 0; ; {
		i := strings.Index(source[start:], word)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(word)
		left := i == 0 || !(source[i-1] == '_' || source[i-1] >= 'a' && source[i-1] <= 'z' || source[i-1] >= '0' && source[i-1] <= '9')
		right := end == len(source) || !(source[end] == '_' || source[end] >= 'a' && source[end] <= 'z' || source[end] >= '0' && source[end] <= '9')
		if left && right {
			return true
		}
		start = end
	}
}

func opaquePowerShell(source string) bool {
	lower := strings.ToLower(source)
	if strings.Contains(lower, "invoke-expression") || containsPowerShellCommand(lower, "iex") {
		return true
	}
	r := []rune(source)
	for i, c := range r {
		if c != '&' && c != '.' {
			continue
		}
		j := i + 1
		for j < len(r) && (r[j] == ' ' || r[j] == '\t' || r[j] == '(' || r[j] == '\'' || r[j] == '"') {
			j++
		}
		if j < len(r) && r[j] == '$' {
			return true
		}
	}
	return false
}
