package agentguard

import (
	"strings"

	"github.com/PHPCraftdream/rush/internal/shell"
)

func policyArgvCandidates(args []string, depth int) [][]string {
	if len(args) == 0 {
		return nil
	}
	if depth > 8 {
		return [][]string{{"\x00rush-opaque-command"}}
	}
	out := [][]string{append([]string(nil), args...)}
	if args[0] == policyDynamicArg {
		return out
	}
	shellHead := isPosixPolicyShell(policyCanonicalExecutable(args[0]))
	// Unconditionally union in the old textual-parser candidates. For shell
	// heads the marker-preserving path owns opacity, so only the sentinel
	// candidates from this union are skipped; every other head keeps them.
	for _, candidate := range shell.CommandCandidatesForPlatform(args, true) {
		if shellHead && isOpaqueCandidateArgv(candidate) {
			continue
		}
		out = append(out, candidate)
	}
	head, dynamic, index := resolveArgvPolicyHead(args)
	if dynamic {
		return append(out, policyOpaqueCandidate())
	}
	if isPosixPolicyShell(head) && index < len(args) {
		out = append(out, policyPosixCandidates(args[index:], depth+1)...)
	}
	if index > 0 && index < len(args) {
		out = append(out, policyArgvCandidates(args[index:], depth+1)...)
	}
	return out
}

const policyOpaqueCandidateLiteral = "\x00rush-opaque-command"

func policyOpaqueCandidate() []string {
	return []string{policyOpaqueCandidateLiteral}
}

func isOpaqueCandidateArgv(candidate []string) bool {
	return len(candidate) == 1 && candidate[0] == policyOpaqueCandidateLiteral
}

func policyPosixCandidates(args []string, depth int) [][]string {
	if depth > 8 {
		return [][]string{{"\x00rush-opaque-command"}}
	}
	out := [][]string{append([]string(nil), args...)}
	payload := policyShellPayload(args)
	if payload == "" {
		return out
	}
	calls, opaque := policyShellCalls(payload)
	if opaque {
		return append(out, []string{"\x00rush-opaque-command"})
	}
	for _, call := range calls {
		out = append(out, policyArgvCandidates(call, depth+1)...)
	}
	return out
}

func isPosixPolicyShell(head string) bool {
	return head == "sh" || head == "bash" || head == "zsh" || head == "dash"
}

func policyShellPayload(args []string) string {
	for i := 1; i < len(args); i++ {
		option := args[i]
		if option == "--" {
			return ""
		}
		if !strings.HasPrefix(option, "-") {
			return ""
		}
		if strings.HasPrefix(option, "--") {
			if option == "--rcfile" || option == "--init-file" || option == "--startup-file" {
				i++
			}
			continue
		}
		cluster := strings.TrimPrefix(option, "-")
		if j := strings.IndexByte(cluster, 'c'); j >= 0 {
			if j == len(cluster)-1 && i+1 < len(args) {
				return args[i+1]
			}
			return cluster[j+1:]
		}
		if strings.ContainsAny(option, "oO") && i+1 < len(args) {
			i++
		}
	}
	return ""
}

func isPolicyPrefix(head string) bool {
	head = policyCanonicalExecutable(head)
	return valueNice[head] || noValueNice[head] || strings.Contains("|env|sudo|xargs|timeout|nice|command|builtin|exec|nohup|time|", "|"+head+"|")
}

func resolveArgvPolicyHead(args []string) (string, bool, int) {
	i := 0
	for i < len(args) && isEnvAssignment(args[i]) {
		i++
	}
	for i < len(args) {
		if args[i] == policyDynamicArg {
			return "", true, i
		}
		head := policyCanonicalExecutable(args[i])
		i++
		if valueNice[head] {
			if i >= len(args) || args[i] == policyDynamicArg {
				return "", true, i
			}
			i++
			continue
		}
		if noValueNice[head] {
			continue
		}
		switch head {
		case "command", "builtin", "exec", "nohup", "time", "nice", "env", "timeout":
			suffix := shell.SkipLauncherPrefixes(args[i-1:])
			if len(suffix) == 0 {
				return "", true, len(args)
			}
			cmdIdx := len(args) - len(suffix)
			for _, token := range args[i-1 : cmdIdx] {
				if token == policyDynamicArg {
					return "", true, cmdIdx
				}
			}
			if suffix[0] == policyDynamicArg {
				return "", true, cmdIdx
			}
			i = cmdIdx
			continue
		case "sudo", "xargs":
		default:
			return head, false, i - 1
		}
		for i < len(args) {
			token := args[i]
			if token == "--" {
				i++
				break
			}
			if head == "env" && isEnvAssignment(token) {
				i++
				continue
			}
			if !strings.HasPrefix(token, "-") {
				break
			}
			if isPrefixOperand(head, token) && !strings.Contains(token, "=") {
				i++
				if i < len(args) && args[i] == policyDynamicArg {
					return "", true, i
				}
				if i < len(args) {
					i++
				}
				continue
			}
			i++
		}
		if head == "timeout" && i < len(args) && !strings.HasPrefix(args[i], "-") {
			if args[i] == policyDynamicArg {
				return "", true, i
			}
			i++
		}
	}
	return "", true, i
}

func isPrefixOperand(head, token string) bool {
	switch head {
	case "timeout":
		return token == "-k" || token == "--kill-after" || token == "-s" || token == "--signal"
	case "nice":
		return token == "-n" || token == "--adjustment"
	case "sudo":
		return strings.Contains("|-u|-g|-h|-p|-C|-T|", "|"+token+"|")
	case "xargs":
		return strings.Contains("|-n|-P|-I|-s|-L|-a|-E|-d|--max-args|--max-procs|--replace|--max-chars|--max-lines|--arg-file|--eof|--delimiter|", "|"+token+"|")
	case "env":
		return token == "-u" || token == "--unset" || token == "-C" || token == "--chdir"
	}
	return false
}
