package agentguard

import (
	"regexp"
	"strings"
)

type inlineOptionSpec struct {
	tool, option         string
	attached, longEquals bool
}

var inlineOptionSpecs = []inlineOptionSpec{{"node", "-pe", true, false}, {"node", "-ep", true, false}, {"node", "-e", true, false}, {"node", "--eval", false, true}, {"node", "-p", true, false}, {"node", "--print", false, true}, {"python", "-c", true, false}, {"python3", "-c", true, false}, {"py", "-c", true, false}}

// parseInlineOption returns inline code and the number of following argv entries consumed.
func parseInlineOption(tool string, args []string, index int) (code string, consumed int, ok bool) {
	if index < 0 || index >= len(args) {
		return "", 0, false
	}
	tool = policyCanonicalExecutable(tool)
	token := args[index]
	for _, spec := range inlineOptionSpecs {
		if spec.tool != tool {
			continue
		}
		if token == spec.option {
			if index+1 >= len(args) {
				return "", 0, true
			}
			return args[index+1], 1, true
		}
		if spec.longEquals && strings.HasPrefix(token, spec.option+"=") {
			return strings.TrimPrefix(token, spec.option+"="), 0, true
		}
		if spec.attached && strings.HasPrefix(token, spec.option) && len(token) > len(spec.option) {
			return token[len(spec.option):], 0, true
		}
	}
	return "", 0, false
}

type inlineCommandPattern struct {
	tool    string
	pattern *regexp.Regexp
	list    bool
}

var inlineCommandPatterns = []inlineCommandPattern{
	{"node", regexp.MustCompile(`(?is)(?:execSync|exec)\s*\(\s*(['"])([^'"]*)['"]`), false},
	{"node", regexp.MustCompile(`(?is)(?:spawnSync|spawn)\s*\(\s*(['"])([^'"]*)['"]\s*,\s*\[([^\]]*)\]`), true},
	{"python", regexp.MustCompile(`(?is)subprocess\.(?:run|call|check_call|check_output|Popen)\s*\(\s*(['"])([^'"]*)['"]`), false},
	{"python", regexp.MustCompile(`(?is)subprocess\.(?:run|call|check_call|check_output|Popen)\s*\(\s*\[([^\]]*)\]`), true},
}

func literalInlineArgvs(tool, code string) [][]string {
	tool = policyCanonicalExecutable(tool)
	var out [][]string
	for _, spec := range inlineCommandPatterns {
		if (spec.tool == "python") != (tool == "python" || tool == "python3" || tool == "py") {
			continue
		}
		for _, match := range spec.pattern.FindAllStringSubmatch(code, -1) {
			if !spec.list {
				if len(match) > 2 {
					calls, opaque := policyShellCalls(match[2])
					if !opaque {
						out = append(out, calls...)
					}
				}
				continue
			}
			if spec.tool == "node" {
				if len(match) < 4 {
					continue
				}
				extra, ok := literalStringArray(match[3])
				if ok {
					out = append(out, append([]string{match[2]}, extra...))
				}
				continue
			}
			if len(match) > 1 {
				argv, ok := literalStringArray(match[1])
				if ok && len(argv) > 0 {
					out = append(out, argv)
				}
			}
		}
	}
	if tool == "node" {
		for _, cmd := range literalNodeExecCommands(code) {
			calls, opaque := policyShellCalls(cmd)
			if !opaque {
				out = append(out, calls...)
			}
		}
	}
	return out
}

func literalStringArray(source string) ([]string, bool) {
	var values []string
	for i := 0; i < len(source); {
		for i < len(source) && (source[i] == ' ' || source[i] == '\t' || source[i] == '\r' || source[i] == '\n' || source[i] == ',') {
			i++
		}
		if i == len(source) {
			break
		}
		q := source[i]
		if q != '\'' && q != '"' {
			return nil, false
		}
		i++
		start := i
		for i < len(source) && source[i] != q {
			if source[i] == '\\' {
				return nil, false
			}
			i++
		}
		if i == len(source) {
			return nil, false
		}
		values = append(values, source[start:i])
		i++
	}
	return values, len(values) > 0
}
