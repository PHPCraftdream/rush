package agentguard

import "strings"

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
