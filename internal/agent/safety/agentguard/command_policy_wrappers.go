package agentguard

import (
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var (
	valueNice   = map[string]bool{"cap": true, "capc": true, "capt": true, "capm": true, "caps": true, "capn": true, "pint": true}
	noValueNice = map[string]bool{"idle": true, "belownormal": true, "abovenormal": true, "high": true, "realtime": true, "uiup": true, "admin": true}
)

const wrapperDepthLimit = 32

func (p CommandPolicy) checkCandidates(candidates [][]string) error {
	seen := make(map[string]struct{})
	var visit func([]string, int) error
	visit = func(args []string, depth int) error {
		if len(args) == 0 {
			return nil
		}
		if depth > wrapperDepthLimit || len(args) == 1 && args[0] == "\x00rush-opaque-command" {
			if len(p.rules) > 0 {
				return &CommandDeniedError{Rule: "opaque-wrapper-command", Detail: "wrapper nesting depth or opaque shell candidate prevented inspection; " + opaqueWrapperDetail}
			}
			return nil
		}
		key := strings.Join(args, "\x00")
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		if reason, opaque := p.checkOpaqueWrapper(args); opaque {
			return &CommandDeniedError{Rule: "opaque-wrapper-command", Detail: "opaque command head or payload " + reason + "; " + opaqueWrapperDetail}
		}
		if rule := matchPolicy(args, p.rules); rule != "" {
			return &CommandDeniedError{Rule: rule}
		}
		for _, nested := range augmentPolicyWrappers(args) {
			if err := visit(nested, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, candidate := range candidates {
		if err := visit(candidate, 0); err != nil {
			return err
		}
	}
	return nil
}

func augmentPolicyWrappers(args []string) [][]string {
	if len(args) == 0 {
		return nil
	}
	var out [][]string
	head := policyCanonicalExecutable(args[0])
	rest := args[1:]
	appendCandidates := func(inner []string) { out = append(out, policyArgvCandidates(inner, 1)...) }
	if valueNice[head] && len(rest) > 1 {
		appendCandidates(rest[1:])
	}
	if noValueNice[head] && len(rest) > 0 {
		appendCandidates(rest)
	}
	if head == "find" {
		for i, token := range rest {
			if (token == "-exec" || token == "-execdir") && i+1 < len(rest) {
				end := i + 1
				for end < len(rest) && rest[end] != ";" && rest[end] != "+" {
					end++
				}
				if end > i+1 {
					appendCandidates(rest[i+1 : end])
				}
			}
		}
	}
	if head == "node" || head == "python" || head == "python3" || head == "py" {
		for i := 0; i < len(rest); i++ {
			if code, consumed, ok := parseInlineOption(head, rest, i); ok {
				for _, argv := range literalInlineArgvs(head, code) {
					out = append(out, policyArgvCandidates(argv, 1)...)
				}
				i += consumed
				continue
			}
			name := rest[i]
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			if commandPolicyOptionFlags[head][name] || strings.HasPrefix(name, "--test-") {
				continue
			}
			if commandPolicyOptionOperands[head][name] {
				if !strings.Contains(rest[i], "=") {
					i++
				}
				continue
			}
			if !strings.HasPrefix(rest[i], "-") {
				break
			}
		}
	}
	if head == "cmd" || head == "cmd.exe" {
		for i, token := range rest {
			if strings.EqualFold(token, "/c") || strings.EqualFold(token, "/k") {
				if i+1 < len(rest) {
					appendCandidates(rest[i+1:])
					appendCandidates([]string{"sh", "-c", strings.Join(rest[i+1:], " ")})
				}
				break
			}
		}
	}
	if head == "sudo" {
		i := 0
		valueOptions := map[string]bool{"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-T": true}
		for i < len(rest) {
			token := rest[i]
			if token == "--" {
				i++
				break
			}
			if !strings.HasPrefix(token, "-") {
				break
			}
			option := token
			if eq := strings.IndexByte(token, '='); eq >= 0 {
				option = token[:eq]
			}
			i++
			if valueOptions[option] && !strings.Contains(token, "=") && i < len(rest) {
				i++
			}
		}
		if i < len(rest) {
			appendCandidates(rest[i:])
		}
	}
	if head == "start" {
		i := 0
		for i < len(rest) && strings.HasPrefix(rest[i], "/") {
			i++
		}
		if i < len(rest) && len(rest)-i > 1 && rest[i] == "" {
			i++ // cmd start may include an empty title argument
		}
		if i < len(rest) {
			appendCandidates(rest[i:])
			if len(rest)-i >= 2 {
				appendCandidates(rest[i+1:])
			}
		}
	}
	if head == "npx" || head == "pnpm" || head == "yarn" || head == "bunx" {
		for i, token := range rest {
			if token == "--" {
				if i+1 < len(rest) {
					appendCandidates(rest[i+1:])
				}
				break
			}
			if !strings.HasPrefix(token, "-") && (head != "pnpm" && head != "yarn" || token == "dlx") {
				if head == "pnpm" || head == "yarn" {
					if i+1 < len(rest) {
						appendCandidates(rest[i+1:])
					}
				} else {
					appendCandidates(rest[i:])
				}
				break
			}
		}
	}
	if head == "xargs" {
		value := map[string]bool{"-n": true, "-P": true, "-I": true, "-s": true, "-L": true, "-a": true, "-E": true, "-d": true, "--max-args": true, "--max-procs": true, "--replace": true, "--max-chars": true, "--max-lines": true, "--arg-file": true, "--eof": true, "--delimiter": true}
		i := 0
		for i < len(rest) {
			token := rest[i]
			if token == "--" {
				i++
				break
			}
			if !strings.HasPrefix(token, "-") {
				break
			}
			option := token
			if eq := strings.IndexByte(token, '='); eq >= 0 {
				option = token[:eq]
			}
			i++
			if value[option] && !strings.Contains(token, "=") && i < len(rest) {
				i++
			}
		}
		if i < len(rest) {
			appendCandidates(rest[i:])
		}
	}
	return out
}

var literalNodeExecPatterns = []*regexp.Regexp{regexp.MustCompile(`(?i)(?:execSync|exec)\s*\(\s*'([^']*)'`), regexp.MustCompile(`(?i)(?:execSync|exec)\s*\(\s*"([^"]*)"`)}

func literalNodeExecCommands(code string) []string {
	var commands []string
	for _, pattern := range literalNodeExecPatterns {
		for _, match := range pattern.FindAllStringSubmatch(code, -1) {
			if len(match) == 2 {
				commands = append(commands, match[1])
			}
		}
	}
	return commands
}

func policyCanonicalExecutable(value string) string {
	value = strings.ReplaceAll(value, `\`, `/`)
	value = strings.ToLower(path.Base(value))
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".com", ".ps1"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return value
}

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
	// A payload word with an expansion (bash -c "... $x") cannot be
	// inspected; do not rely on the parser to trip over the marker.
	if strings.Contains(payload, policyDynamicArg) {
		return "(dynamic payload)", true
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
	runes := []rune(source)
	for i, r := range runes {
		if r != '&' && r != '.' {
			continue
		}
		j := i + 1
		for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t' || runes[j] == '(' || runes[j] == '\'' || runes[j] == '"') {
			j++
		}
		if j < len(runes) && runes[j] == '$' {
			return true
		}
	}
	return false
}
