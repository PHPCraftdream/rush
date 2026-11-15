package agentguard

import (
	"regexp"
	"strings"
)

// Option metadata for tool argv scanners. Options with operands consume the
// following argv token unless supplied as --option=value.
var commandPolicyOptionOperands = map[string]map[string]bool{
	"npm": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "pnpm": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "yarn": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "bun": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "npx": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"),
	"python": optionSet("-W -X -c -m"), "python3": optionSet("-W -X -c -m"), "py": optionSet("-W -X -c -m"),
	"cargo": optionSet("--manifest-path -Z --config -C --color --target-dir -j"), "go": optionSet("-C"),
	"node": optionSet("--require -r --import --loader --env-file -e -p"),
}

var commandPolicyOptionFlags = map[string]map[string]bool{
	"npm": optionSet("--workspaces"), "pnpm": optionSet("--workspaces"), "yarn": optionSet("--workspaces"), "bun": optionSet("--workspaces"), "npx": optionSet("--workspaces"),
	"python": optionSet("-I -E -s -S -B -u -O -OO -q -v -x -b -d -R -P"), "python3": optionSet("-I -E -s -S -B -u -O -OO -q -v -x -b -d -R -P"), "py": optionSet("-3 -I -E -s -S -B -u -O -OO -q -v -x -b -d -R -P"),
	"cargo": optionSet("+toolchain --"), "go": optionSet("-x -n -v"), "node": optionSet("--test"),
}

func optionSet(words string) map[string]bool {
	set := make(map[string]bool)
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}

// scanToolArgs returns recognized positional tokens and whether an unknown
// option was encountered. Unknown options leave the remaining argv available
// for conservative forbidden-token scanning.
func scanToolArgs(tool string, args []string) ([]string, bool) {
	operands, flags := commandPolicyOptionOperands[tool], commandPolicyOptionFlags[tool]
	var positional []string
	unknown := false
	for i := 0; i < len(args); i++ {
		token := args[i]
		if _, _, ok := parseInlineOption(tool, args, i); ok {
			return positional, unknown
		}
		if strings.HasPrefix(token, "+") && tool == "cargo" {
			continue
		}
		if token == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(token, "-") {
			positional = append(positional, token)
			if tool == "node" {
				return positional, unknown
			}
			continue
		}
		name := token
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if flags[name] {
			if tool == "node" && name == "--test" {
				positional = append(positional, token)
			}
			continue
		}
		if tool == "node" && strings.HasPrefix(name, "--test-") {
			positional = append(positional, token)
			continue
		}
		if operands[name] {
			if (tool == "python" || tool == "python3" || tool == "py") && name == "-m" {
				positional = append(positional, token)
				if i+1 < len(args) {
					positional = append(positional, args[i+1])
				}
				return positional, unknown
			}
			if !strings.Contains(token, "=") && i+1 < len(args) {
				i++
			}
			continue
		}
		unknown = true
		positional = append(positional, args[i:]...)
		break
	}
	return positional, unknown
}

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
	bare    bool
}

var inlineCommandPatterns = []inlineCommandPattern{
	{"node", regexp.MustCompile(`(?is)(?:execSync|exec)\s*\(\s*(['"])([^'"]*)['"]`), false, false},
	{"node", regexp.MustCompile(`(?is)(?:spawnSync|spawn)\s*\(\s*(['"])([^'"]*)['"]\s*,\s*\[([^\]]*)\]`), true, false},
	// Bare matches a direct `)` or a comma NOT followed by `[` (RE2 has no
	// lookahead; this alternation is the equivalent). `, [` is left to the
	// array branch so empty-array calls are observable there.
	{"node", regexp.MustCompile(`(?is)(?:spawnSync|spawn|execFile|execFileSync)\s*\(\s*(['"])([^'"]*)['"]\s*(?:\)|,\s*[^\[])`), false, true},
	{"python", regexp.MustCompile(`(?is)subprocess\.(?:run|call|check_call|check_output|Popen)\s*\(\s*(['"])([^'"]*)['"]`), false, false},
	{"python", regexp.MustCompile(`(?is)subprocess\.(?:run|call|check_call|check_output|Popen)\s*\(\s*\[([^\]]*)\]`), true, false},
}

func literalInlineArgvs(tool, code string) [][]string {
	tool = policyCanonicalExecutable(tool)
	var out [][]string
	for _, spec := range inlineCommandPatterns {
		if (spec.tool == "python") != (tool == "python" || tool == "python3" || tool == "py") {
			continue
		}
		for _, match := range spec.pattern.FindAllStringSubmatch(code, -1) {
			if spec.bare {
				if len(match) > 2 {
					out = append(out, []string{match[2]})
				}
				continue
			}
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
	return values, true
}
