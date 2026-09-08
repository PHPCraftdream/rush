package tools

import (
	"runtime"
	"strings"

	"github.com/PHPCraftdream/rush/internal/shell"
)

var safeNoArgCommands = map[string]struct{}{
	"cal": {}, "date": {}, "df": {}, "du": {}, "free": {}, "groups": {},
	"hostname": {}, "id": {}, "printenv": {}, "ps": {}, "pwd": {}, "top": {},
	"type": {}, "uname": {}, "uptime": {}, "whatis": {}, "whereis": {},
	"which": {}, "whoami": {},
}

// isSafeReadOnlyCommand accepts one parsed command with literal argv. Every
// command and option is explicitly classified; unknown forms require a
// permission request.
func isSafeReadOnlyCommand(command string) bool {
	args, ok := shell.ParseSimpleCommand(command)
	if !ok {
		return false
	}
	if commandNameMatches(args[0], "git") {
		return isSafeGitCommand(args[1:])
	}
	if commandNameMatches(args[0], "echo") {
		return true
	}
	if commandNameMatches(args[0], "ls") {
		return safeLSArgs(args[1:])
	}
	if len(args) != 1 {
		return false
	}
	for safe := range safeNoArgCommands {
		if commandNameMatches(args[0], safe) {
			return true
		}
	}
	return false
}

func commandNameMatches(actual, expected string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(actual, expected)
	}
	return actual == expected
}

func safeLSArgs(args []string) bool {
	const shortOptions = "aAbBcCdDfFghHiLlmNnopqQrRrsStuUvxX1"
	for _, arg := range args {
		if arg == "--" {
			return true
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		if strings.HasPrefix(arg, "--") {
			if arg == "--all" || arg == "--almost-all" || arg == "--author" ||
				arg == "--color" || arg == "--classify" || arg == "--directory" ||
				arg == "--dereference" || arg == "--human-readable" || arg == "--inode" ||
				arg == "--reverse" || arg == "--recursive" || arg == "--size" ||
				arg == "--sort" || arg == "--time" {
				continue
			}
			if strings.HasPrefix(arg, "--color=") {
				value := strings.TrimPrefix(arg, "--color=")
				if value == "always" || value == "auto" || value == "never" {
					continue
				}
			}
			return false
		}
		for _, option := range arg[1:] {
			if !strings.ContainsRune(shortOptions, option) {
				return false
			}
		}
	}
	return true
}

func isSafeGitCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}

	switch strings.ToLower(args[0]) {
	case "blame", "describe", "grep", "ls-files", "rev-parse", "shortlog", "show", "diff", "log", "status":
		return safeGitReadArgs(args[1:])
	case "branch":
		return safeGitBranchArgs(args[1:])
	case "config":
		return safeGitConfigArgs(args[1:])
	case "remote":
		return safeGitRemoteArgs(args[1:])
	case "tag":
		return safeGitTagArgs(args[1:])
	default:
		return false
	}
}

// safeGitReadArgs intentionally admits no options. A Git read command's
// options can invoke pagers/helpers or write output; callers can authorize
// those forms explicitly when needed.
func safeGitReadArgs(args []string) bool {
	for _, arg := range args {
		if arg != "--" && strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return true
}

func safeGitBranchArgs(args []string) bool {
	list := false
	for i := 0; i < len(args); i++ {
		arg := strings.ToLower(args[i])
		switch {
		case arg == "--":
			return list
		case arg == "--list" || arg == "-l" || strings.HasPrefix(arg, "--list="):
			list = true
		case arg == "--show-current":
		case arg == "--all", arg == "--remotes", arg == "-a", arg == "-r":
			list = true
		case arg == "--contains", arg == "--no-contains", arg == "--merged", arg == "--no-merged", arg == "--points-at", arg == "--sort", arg == "--format":
			if i+1 >= len(args) {
				return false
			}
			i++
			list = true
		case strings.HasPrefix(arg, "--contains="), strings.HasPrefix(arg, "--no-contains="), strings.HasPrefix(arg, "--merged="), strings.HasPrefix(arg, "--no-merged="), strings.HasPrefix(arg, "--points-at="), strings.HasPrefix(arg, "--sort="), strings.HasPrefix(arg, "--format="):
			list = true
		case arg == "--column", arg == "--no-column", arg == "--color", arg == "--no-color" || strings.HasPrefix(arg, "--column=") || strings.HasPrefix(arg, "--color="):
			list = true
		case strings.HasPrefix(arg, "-"):
			return false
		case !list:
			return false
		}
	}
	return true
}

func safeGitConfigArgs(args []string) bool {
	read := false
	for _, arg := range args {
		lower := strings.ToLower(arg)
		switch {
		case lower == "--get", lower == "--get-all", lower == "--get-regexp", lower == "--get-urlmatch", lower == "--list", lower == "-l":
			read = true
		case lower == "--global", lower == "--system", lower == "--local", lower == "--worktree", lower == "--null", lower == "-z", lower == "--name-only", lower == "--show-origin", lower == "--show-scope", lower == "--fixed-value":
		case strings.HasPrefix(lower, "--type="), strings.HasPrefix(lower, "--file="), strings.HasPrefix(lower, "--blob="):
		case strings.HasPrefix(lower, "-"):
			return false
		case !read:
			return false
		}
	}
	return read
}

func safeGitRemoteArgs(args []string) bool {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "-v" || args[0] == "--verbose")) {
		return true
	}
	if args[0] == "show" {
		if len(args) < 2 || args[1] != "-n" {
			return false
		}
		for _, arg := range args[2:] {
			if strings.HasPrefix(arg, "-") {
				return false
			}
		}
		return true
	}

	mode := ""
	for i := 0; i < len(args); i++ {
		arg := strings.ToLower(args[i])
		switch {
		case mode == "" && (arg == "get-url" || arg == "get-branches"):
			mode = arg
		case mode == "get-url" && (arg == "--all" || arg == "--push"):
		case mode == "get-url" && (arg == "--protocol=ssh" || arg == "--protocol=http" || arg == "--protocol=https"):
		case mode != "" && !strings.HasPrefix(arg, "-"):
		case strings.HasPrefix(arg, "-"):
			return false
		default:
			return false
		}
	}
	return mode != ""
}

func safeGitTagArgs(args []string) bool {
	list := false
	for i := 0; i < len(args); i++ {
		arg := strings.ToLower(args[i])
		switch {
		case arg == "--":
			return list
		case arg == "-l" || arg == "--list" || strings.HasPrefix(arg, "--list="):
			list = true
		case arg == "--contains", arg == "--no-contains", arg == "--merged", arg == "--no-merged", arg == "--points-at", arg == "--sort", arg == "--format":
			if i+1 >= len(args) {
				return false
			}
			i++
			list = true
		case strings.HasPrefix(arg, "--contains="), strings.HasPrefix(arg, "--no-contains="), strings.HasPrefix(arg, "--merged="), strings.HasPrefix(arg, "--no-merged="), strings.HasPrefix(arg, "--points-at="), strings.HasPrefix(arg, "--sort="), strings.HasPrefix(arg, "--format="):
			list = true
		case isGitTagNumberFlag(arg):
			list = true
		case strings.HasPrefix(arg, "-"):
			return false
		case !list:
			return false
		}
	}
	return true
}

func isGitTagNumberFlag(arg string) bool {
	if arg == "-n" {
		return true
	}
	if !strings.HasPrefix(arg, "-n") || len(arg) == 2 {
		return false
	}
	for _, r := range arg[2:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func init() {
	if runtime.GOOS == "windows" {
		for _, command := range []string{"ipconfig", "nslookup", "ping", "systeminfo", "tasklist", "where"} {
			safeNoArgCommands[command] = struct{}{}
		}
	}
}
