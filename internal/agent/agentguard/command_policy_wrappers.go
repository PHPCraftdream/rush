package agentguard

import (
	"path"
	"regexp"
	"strings"

	"github.com/PHPCraftdream/rush/internal/shell"
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
		if depth > wrapperDepthLimit || len(args) == 1 && (args[0] == "\x00rush-opaque-command") {
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
	appendCandidates := func(inner []string) {
		out = append(out, shell.CommandCandidatesForPlatform(inner, true)...)
	}
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
	if head == "node" {
		for i := 0; i < len(rest); i++ {
			arg := rest[i]
			name := arg
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			if commandPolicyOptionFlags["node"][name] || strings.HasPrefix(name, "--test-") {
				continue
			}
			if name == "-e" || name == "--eval" || name == "-p" || name == "--print" {
				if i+1 < len(rest) {
					for _, command := range literalNodeExecCommands(rest[i+1]) {
						appendCandidates([]string{"sh", "-c", command})
					}
				}
				i++
				continue
			}
			if operands := commandPolicyOptionOperands["node"]; operands[name] {
				if !strings.Contains(arg, "=") {
					i++
				}
				continue
			}
			if !strings.HasPrefix(arg, "-") {
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

var literalNodeExecPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:execSync|exec)\s*\(\s*'([^']*)'`),
	regexp.MustCompile(`(?i)(?:execSync|exec)\s*\(\s*"([^"]*)"`),
}

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
	value = strings.ReplaceAll(value, `\`, "/")
	value = strings.ToLower(path.Base(value))
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".com", ".ps1"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return value
}
