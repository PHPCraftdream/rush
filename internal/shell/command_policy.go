package shell

import (
	"runtime"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/syntax"
)

// BannedCommands returns the canonical list shown in shell tool descriptions.
// The returned slice is independent and may be modified by the caller.
func BannedCommands() []string {
	return append([]string(nil), bannedCommands...)
}

// CanonicalBlockFuncs returns the hard-deny rules used by every Rush command
// execution surface. A fresh slice is returned for each shell instance.
func CanonicalBlockFuncs() []BlockFunc {
	return []BlockFunc{
		CommandsBlocker(bannedCommands),
		ArgumentsBlocker("apk", []string{"add"}, nil),
		ArgumentsBlocker("apt", []string{"install"}, nil),
		ArgumentsBlocker("apt-get", []string{"install"}, nil),
		ArgumentsBlocker("dnf", []string{"install"}, nil),
		ArgumentsBlocker("pacman", nil, []string{"-S"}),
		ArgumentsBlocker("pkg", []string{"install"}, nil),
		ArgumentsBlocker("yum", []string{"install"}, nil),
		ArgumentsBlocker("zypper", []string{"install"}, nil),
		ArgumentsBlocker("brew", []string{"install"}, nil),
		ArgumentsBlocker("cargo", []string{"install"}, nil),
		ArgumentsBlocker("gem", []string{"install"}, nil),
		ArgumentsBlocker("go", []string{"install"}, nil),
		ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
		ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
		ArgumentsBlocker("pip", []string{"install"}, []string{"--user"}),
		ArgumentsBlocker("pip3", []string{"install"}, []string{"--user"}),
		ArgumentsBlocker("pnpm", []string{"add"}, []string{"--global"}),
		ArgumentsBlocker("pnpm", []string{"add"}, []string{"-g"}),
		ArgumentsBlocker("yarn", []string{"global", "add"}, nil),
		ArgumentsBlocker("go", []string{"test"}, []string{"-exec"}),
	}
}

var bannedCommands = []string{
	"alias", "aria2c", "axel", "chrome", "curl", "curlie", "firefox",
	"http-prompt", "httpie", "links", "lynx", "nc", "safari", "scp",
	"ssh", "telnet", "w3m", "wget", "xh",
	"doas", "su", "sudo",
	"apk", "apt", "apt-cache", "apt-get", "dnf", "dpkg", "emerge",
	"home-manager", "makepkg", "opkg", "pacman", "paru", "pkg", "pkg_add",
	"pkg_delete", "portage", "rpm", "yay", "yum", "zypper",
	"at", "batch", "chkconfig", "crontab", "fdisk", "mkfs", "mount",
	"parted", "service", "systemctl", "umount",
	"firewall-cmd", "ifconfig", "ip", "iptables", "netstat", "pfctl",
	"route", "ufw",
}

// CommandsBlocker matches the executable basename, independent of path,
// slash spelling, Windows case, or common executable/script suffixes. It
// also checks commands embedded in established launchers and shell -c forms.
func CommandsBlocker(cmds []string) BlockFunc {
	return CommandsBlockerForPlatform(cmds, runtime.GOOS == "windows")
}

// CommandsBlockerForPlatform is the platform-pure form used by tests and
// cross-platform policy checks. Windows folds executable case; Unix does not.
func CommandsBlockerForPlatform(cmds []string, windows bool) BlockFunc {
	banned := make(map[string]struct{}, len(cmds))
	for _, cmd := range cmds {
		if name := canonicalExecutableForPlatform(cmd, windows); name != "" {
			banned[name] = struct{}{}
		}
	}
	return func(args []string) bool {
		for _, candidate := range commandCandidates(args, 0, windows) {
			if isOpaqueCandidate(candidate) {
				return true
			}
			if _, ok := banned[canonicalExecutableForPlatform(candidate[0], windows)]; ok {
				return true
			}
		}
		return false
	}
}

// ArgumentsBlocker matches a command after launcher unwrapping, preserving
// the existing positional-argument and required-flag semantics.
func ArgumentsBlocker(cmd string, args []string, flags []string) BlockFunc {
	return ArgumentsBlockerForPlatform(cmd, args, flags, runtime.GOOS == "windows")
}

// ArgumentsBlockerForPlatform is the platform-pure form of ArgumentsBlocker.
func ArgumentsBlockerForPlatform(cmd string, args []string, flags []string, windows bool) BlockFunc {
	want := canonicalExecutableForPlatform(cmd, windows)
	return func(parts []string) bool {
		for _, candidate := range commandCandidates(parts, 0, windows) {
			if isOpaqueCandidate(candidate) {
				return true
			}
			if canonicalExecutableForPlatform(candidate[0], windows) != want {
				continue
			}
			argParts, flagParts := splitArgsFlags(candidate[1:])
			if len(argParts) < len(args) || len(flagParts) < len(flags) {
				continue
			}
			if equalStrings(argParts[:len(args)], args) && containsAll(flagParts, flags) {
				return true
			}
		}
		return false
	}
}

// CommandCandidates returns argv candidates for a command and its supported
// launchers, including nested shell -c/-Command payloads. It is used by
// other execution guards so they share this parser instead of reimplementing
// wrapper handling.
func CommandCandidates(args []string) [][]string {
	return CommandCandidatesForPlatform(args, runtime.GOOS == "windows")
}

// CommandCandidatesForPlatform is the platform-pure form of CommandCandidates.
func CommandCandidatesForPlatform(args []string, windows bool) [][]string {
	return commandCandidates(args, 0, windows)
}

// CommandBlocked performs the textual portion of the canonical policy. The
// shell runtime remains authoritative: it catches values produced by runtime
// expansion after this preflight has necessarily run.
func CommandBlocked(command string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	calls, _ := commandCalls(file)
	for _, call := range calls {
		for _, blocker := range CanonicalBlockFuncs() {
			if blocker(call) {
				return true
			}
		}
	}
	return false
}

func commandCalls(file *syntax.File) ([][]string, bool) {
	var calls [][]string
	opaque := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		args := make([]string, len(call.Args))
		for i, word := range call.Args {
			value, ok := literalWord(word.Parts)
			if !ok {
				opaque = true
				return true
			}
			args[i] = value
		}
		calls = append(calls, args)
		return true
	})
	return calls, opaque
}

func literalWord(parts []syntax.WordPart) (string, bool) {
	var value strings.Builder
	for _, part := range parts {
		switch part := part.(type) {
		case *syntax.Lit:
			value.WriteString(part.Value)
		case *syntax.SglQuoted:
			value.WriteString(part.Value)
		case *syntax.DblQuoted:
			inner, ok := literalWord(part.Parts)
			if !ok {
				return "", false
			}
			value.WriteString(inner)
		default:
			return "", false
		}
	}
	return value.String(), true
}

func commandCandidates(args []string, depth int, windows bool) [][]string {
	if len(args) == 0 {
		return nil
	}
	if depth > 8 {
		return [][]string{{opaqueCandidate}}
	}
	if canonicalExecutableForPlatform(args[0], windows) == "&" {
		if len(args) < 2 || args[1] == "" || strings.HasPrefix(args[1], "$") {
			return [][]string{{opaqueCandidate}}
		}
		return commandCandidates(args[1:], depth, windows)
	}
	out := [][]string{append([]string(nil), args...)}
	view := unwrapCommand(args, windows)
	if view.head == "" {
		return out
	}
	if view.opaque {
		return append(out, []string{opaqueCandidate})
	}
	if view.head != canonicalExecutableForPlatform(args[0], windows) || len(view.nested) > 0 {
		candidate := append([]string{view.head}, view.rest...)
		out = append(out, candidate)
	}
	for _, source := range view.nested {
		file, err := syntax.NewParser().Parse(strings.NewReader(source), "")
		var calls [][]string
		opaque := false
		if err == nil {
			calls, opaque = commandCalls(file)
		}
		if err != nil || (len(calls) == 1 && len(calls[0]) == 1 && calls[0][0] == "&") {
			if strings.HasPrefix(strings.TrimSpace(source), "&") {
				calls = nil
				if call, safe := fallbackCommandTokens(source); safe && len(call) > 1 {
					calls = append(calls, call)
				} else {
					opaque = true
				}
			} else {
				opaque = true
			}
		}
		if opaque {
			out = append(out, []string{opaqueCandidate})
		}
		for _, call := range calls {
			out = append(out, commandCandidates(call, depth+1, windows)...)
		}
	}
	return out
}

func fallbackCommandTokens(source string) ([]string, bool) {
	var fields []string
	var current strings.Builder
	inSingle, inDouble := false, false
	quoteClosed := false
	flush := func() {
		if current.Len() > 0 {
			fields = append(fields, current.String())
			current.Reset()
		}
		quoteClosed = false
	}
	runes := []rune(strings.TrimSpace(source))
	if len(runes) == 0 || runes[0] != '&' || (len(runes) > 1 && !unicode.IsSpace(runes[1])) {
		return nil, false
	}
	for i := 1; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'' && !inDouble:
			if !inSingle {
				if current.Len() > 0 || quoteClosed {
					return nil, false
				}
				inSingle = true
			} else if i+1 < len(runes) && runes[i+1] == '\'' {
				current.WriteRune('\'')
				i++
			} else {
				inSingle = false
				quoteClosed = true
			}
		case r == '"' && !inSingle:
			if !inDouble {
				if current.Len() > 0 || quoteClosed {
					return nil, false
				}
				inDouble = true
			} else {
				inDouble = false
				quoteClosed = true
			}
		case unicode.IsSpace(r) && !inSingle && !inDouble:
			flush()
		case quoteClosed:
			return nil, false
		case (r == '$' || r == '`' || r == '@' || r == '(' || r == ')' || r == '+' || r == ';' || r == '|' || r == '<' || r == '>') && !inSingle:
			return nil, false
		default:
			if !inSingle && !inDouble && !safePowerShellUnquotedRune(r) {
				return nil, false
			}
			current.WriteRune(r)
		}
	}
	if inSingle || inDouble {
		return nil, false
	}
	flush()
	if len(fields) == 0 || fields[0] == "" {
		return nil, false
	}
	return append([]string{"&"}, fields...), true
}

func safePowerShellUnquotedRune(r rune) bool {
	return r == '_' || r == '-' || r == '.' || r == '/' || r == '\\' || r == ':' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

type commandView struct {
	head   string
	rest   []string
	nested []string
	opaque bool
}

const opaqueCandidate = "\x00rush-opaque-command"

func unwrapCommand(tokens []string, windows bool) commandView {
	i := 0
	for i < len(tokens) && isEnvAssignment(tokens[i]) {
		i++
	}
	if i >= len(tokens) {
		return commandView{}
	}
	head := canonicalExecutableForPlatform(tokens[i], windows)
	rest := tokens[i+1:]
	var nested []string
	for {
		switch head {
		case "env":
			var envNested []string
			rest, envNested = consumeEnv(rest)
			nested = append(nested, envNested...)
			if len(rest) == 0 {
				return commandView{head: head, rest: rest, nested: nested}
			}
		case "nice":
			rest = consumeOptions(rest, map[string]bool{"-n": true, "--adjustment": true})
			if len(rest) == 0 {
				return commandView{head: head, rest: rest}
			}
		case "timeout":
			rest = consumeOptions(rest, map[string]bool{"-k": true, "--kill-after": true, "-s": true, "--signal": true})
			if len(rest) == 0 {
				return commandView{head: head, rest: rest}
			}
			rest = rest[1:] // GNU timeout duration.
			if len(rest) == 0 {
				return commandView{head: head, rest: rest}
			}
		case "command":
			rest = consumeOptions(rest, map[string]bool{})
			if len(rest) == 0 {
				return commandView{head: head, rest: rest}
			}
		case "exec", "nohup":
			rest = consumeOptions(rest, map[string]bool{"-a": true})
			if len(rest) == 0 {
				return commandView{head: head, rest: rest}
			}
		case "time":
			rest = consumeOptions(rest, map[string]bool{
				"-f": true, "--format": true, "-o": true, "--output": true,
			})
			if len(rest) == 0 {
				return commandView{head: head, rest: rest}
			}
		default:
			view := shellCommandView(head, rest)
			view.nested = append(nested, view.nested...)
			return view
		}
		head = canonicalExecutableForPlatform(rest[0], windows)
		rest = rest[1:]
	}
}

func shellCommandView(head string, rest []string) commandView {
	if isPosixShell(head) {
		if source, found, malformed := posixShellSource(rest); found {
			if malformed {
				return commandView{head: head, rest: rest, opaque: true}
			}
			return commandView{head: head, rest: rest, nested: []string{source}}
		}
	}
	if isCmdShell(head) {
		for i, token := range rest {
			lower := strings.ToLower(token)
			if strings.HasPrefix(token, "/") {
				if lower == "/c" || lower == "/k" {
					if i+1 >= len(rest) {
						return commandView{head: head, rest: rest, opaque: true}
					}
					return commandView{head: head, rest: rest, nested: []string{rest[i+1]}}
				}
				continue
			}
			break
		}
	}
	if isPowerShell(head) {
		return powerShellCommandView(head, rest)
	}
	return commandView{head: head, rest: rest}
}

// posixShellSource recognizes shell -c even when c is in a short-option
// cluster (bash -lc, sh -xc). It stops at the first operand so a script
// filename or ordinary argument cannot be mistaken for a command source.
func posixShellSource(rest []string) (source string, found, malformed bool) {
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		if token == "--" || token == "" || !strings.HasPrefix(token, "-") || token == "-" {
			return "", false, false
		}
		if strings.HasPrefix(token, "--") {
			if posixLongValueOption[strings.ToLower(token)] && i+1 < len(rest) {
				i++
			}
			continue
		}
		cluster := token[1:]
		for j := 0; j < len(cluster); j++ {
			switch cluster[j] {
			case 'o', 'O':
				if j+1 == len(cluster) && i+1 < len(rest) {
					i++
				}
				j = len(cluster)
			case 'c':
				if j+1 < len(cluster) {
					return cluster[j+1:], true, false
				}
				if i+1 >= len(rest) {
					return "", true, true
				}
				return rest[i+1], true, false
			}
		}
	}
	return "", false, false
}

var posixLongValueOption = map[string]bool{
	"--init-file": true, "--rcfile": true, "--startup-file": true,
}

func powerShellCommandView(head string, rest []string) commandView {
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		lower := strings.ToLower(token)
		if lower == "--" || token == "" || !strings.HasPrefix(token, "-") {
			return commandView{head: head, rest: rest}
		}
		switch lower {
		case "-command", "-c":
			if i+1 >= len(rest) {
				return commandView{head: head, rest: rest, opaque: true}
			}
			return commandView{head: head, rest: rest, nested: []string{rest[i+1]}}
		case "-encodedcommand", "-enc", "-e":
			if i+1 >= len(rest) {
				return commandView{head: head, rest: rest, opaque: true}
			}
			decoded, ok := DecodePowerShellEncodedCommandWithUTF8(rest[i+1])
			if !ok {
				return commandView{head: head, rest: rest, opaque: true}
			}
			return commandView{head: head, rest: rest, nested: []string{decoded}}
		}
		if powerShellValueOption[lower] && i+1 < len(rest) {
			i++
		}
	}
	return commandView{head: head, rest: rest}
}

var powerShellValueOption = map[string]bool{
	"-configurationname": true, "-executionpolicy": true, "-inputformat": true,
	"-outputformat": true, "-workingdirectory": true, "-windowstyle": true,
	"-version": true,
}

func isOpaqueCandidate(candidate []string) bool {
	return len(candidate) == 1 && candidate[0] == opaqueCandidate
}

func consumeEnv(tokens []string) ([]string, []string) {
	var nested []string
	for len(tokens) > 0 {
		token := tokens[0]
		if token == "--" {
			return tokens[1:], nested
		}
		if isEnvAssignment(token) {
			tokens = tokens[1:]
			continue
		}
		if strings.HasPrefix(token, "-") {
			if strings.HasPrefix(token, "--split-string=") {
				nested = append(nested, strings.TrimPrefix(token, "--split-string="))
				tokens = tokens[1:]
				continue
			}
			if token == "-S" || token == "--split-string" {
				if len(tokens) > 1 {
					nested = append(nested, tokens[1])
					tokens = tokens[2:]
					continue
				}
			}
			if token == "-u" || token == "--unset" || token == "-C" || token == "--chdir" {
				if len(tokens) > 1 {
					tokens = tokens[2:]
				} else {
					tokens = tokens[1:]
				}
				continue
			}
			tokens = tokens[1:]
			continue
		}
		break
	}
	return tokens, nested
}

func consumeOptions(tokens []string, valueOptions map[string]bool) []string {
	for len(tokens) > 0 && strings.HasPrefix(tokens[0], "-") {
		token := tokens[0]
		tokens = tokens[1:]
		if valueOptions[token] && len(tokens) > 0 && !strings.Contains(token, "=") {
			tokens = tokens[1:]
		}
	}
	return tokens
}

// CanonicalExecutableForPlatform strips path and known executable/script
// suffixes. Only Windows folds case; the explicit target makes the behavior
// testable without launching commands on another operating system.
func CanonicalExecutableForPlatform(name string, windows bool) string {
	if i := strings.LastIndexAny(name, `/\\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Trim(name, `"'`)
	if windows {
		name = strings.ToLower(name)
	}
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".com", ".ps1", ".sh", ".bash", ".zsh", ".ksh", ".fish"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func canonicalExecutableForPlatform(name string, windows bool) string {
	return CanonicalExecutableForPlatform(name, windows)
}

func isEnvAssignment(token string) bool {
	i := strings.IndexByte(token, '=')
	if i <= 0 {
		return false
	}
	for j, r := range token[:i] {
		if (j == 0 && !(r == '_' || unicode.IsLetter(r))) ||
			(j > 0 && !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))) {
			return false
		}
	}
	return true
}

func isPosixShell(name string) bool {
	switch name {
	case "bash", "sh", "dash", "zsh", "ksh", "fish", "nu":
		return true
	default:
		return false
	}
}

func isCmdShell(name string) bool { return name == "cmd" }

func isPowerShell(name string) bool {
	switch name {
	case "powershell", "pwsh":
		return true
	default:
		return false
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsAll(have, want []string) bool {
	for _, needed := range want {
		found := false
		for _, got := range have {
			if got == needed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func splitArgsFlags(parts []string) (args []string, flags []string) {
	args = make([]string, 0, len(parts))
	flags = make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.HasPrefix(part, "-") {
			flag := part
			if before, _, ok := strings.Cut(part, "="); ok {
				flag = before
			}
			flags = append(flags, flag)
		} else {
			args = append(args, part)
		}
	}
	return args, flags
}
