package agentguard

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const opaqueWrapperDetail = "under RUSH_DENY_EXEC dynamic commands are refused because tests/builds run only in GitHub CI; use a literal allowed command"

func (p CommandPolicy) checkOpaqueWrapper(args []string) (string, bool) {
	if len(p.rules) == 0 || len(args) == 0 {
		return "", false
	}
	head := policyCanonicalExecutable(args[0])
	payload, mode := "", ""
	switch head {
	case "sh", "bash", "zsh", "dash":
		for i := 1; i < len(args); i++ {
			option := args[i]
			if option == "--" {
				break
			}
			if !strings.HasPrefix(option, "-") {
				break
			}
			if strings.HasPrefix(option, "--") {
				if option == "--rcfile" || option == "--init-file" || option == "--startup-file" {
					i++
				}
				continue
			}
			cluster := strings.TrimPrefix(option, "-")
			if strings.Contains(cluster, "c") {
				if i+1 < len(args) {
					payload, mode = args[i+1], "sh"
				}
				break
			}
			if strings.Contains(cluster, "o") || strings.Contains(cluster, "O") {
				if i+1 < len(args) {
					i++
				}
			}
		}
	case "powershell", "pwsh":
		for i := 1; i < len(args); i++ {
			if strings.EqualFold(args[i], "-File") {
				return "", false
			}
			if strings.EqualFold(args[i], "-EncodedCommand") || strings.EqualFold(args[i], "-enc") {
				return args[i], true
			}
			if strings.EqualFold(args[i], "-Command") || strings.EqualFold(args[i], "-c") {
				payload, mode = strings.Join(args[i+1:], " "), "ps"
				break
			}
			if strings.EqualFold(args[i], "-ExecutionPolicy") || strings.EqualFold(args[i], "-File") || strings.EqualFold(args[i], "-WindowStyle") {
				i++
				continue
			}
			if !strings.HasPrefix(args[i], "-") {
				break
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
		if opaquePowerShell(payload) {
			return payload, true
		}
		return "", false
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
		name, literal := policyLiteralWord(call.Args[0].Parts)
		if !literal {
			reason = payload
			opaque = true
			return true
		}
		name = strings.ToLower(name)
		if name == "eval" || name == "source" || name == "." {
			opaque, reason = true, name
		}
		return true
	})
	return reason, opaque
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
			value, ok := policyLiteralWord(p.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(value)
		default:
			return "", false
		}
	}
	return b.String(), true
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
