package shpolicy

import (
	"encoding/base64"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func TestArgumentsBlocker(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		args        []string
		flags       []string
		input       []string
		shouldBlock bool
	}{
		// Basic command blocking
		{
			name:        "block exact command match",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"npm", "install", "package"},
			shouldBlock: true,
		},
		{
			name:        "allow different command",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"yarn", "install", "package"},
			shouldBlock: false,
		},
		{
			name:        "allow different subcommand",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"npm", "list"},
			shouldBlock: false,
		},

		// Flag-based blocking
		{
			name:        "block with single flag",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       []string{"-g"},
			input:       []string{"npm", "install", "-g", "typescript"},
			shouldBlock: true,
		},
		{
			name:        "block with flag in different position",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       []string{"-g"},
			input:       []string{"npm", "install", "typescript", "-g"},
			shouldBlock: true,
		},
		{
			name:        "allow without required flag",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       []string{"-g"},
			input:       []string{"npm", "install", "typescript"},
			shouldBlock: false,
		},
		{
			name:        "block with multiple flags",
			cmd:         "pip",
			args:        []string{"install"},
			flags:       []string{"--user"},
			input:       []string{"pip", "install", "--user", "--upgrade", "package"},
			shouldBlock: true,
		},

		// Complex argument patterns
		{
			name:        "block multi-arg subcommand",
			cmd:         "yarn",
			args:        []string{"global", "add"},
			flags:       nil,
			input:       []string{"yarn", "global", "add", "typescript"},
			shouldBlock: true,
		},
		{
			name:        "allow partial multi-arg match",
			cmd:         "yarn",
			args:        []string{"global", "add"},
			flags:       nil,
			input:       []string{"yarn", "global", "list"},
			shouldBlock: false,
		},

		// Edge cases
		{
			name:        "handle empty input",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{},
			shouldBlock: false,
		},
		{
			name:        "handle command only",
			cmd:         "npm",
			args:        []string{"install"},
			flags:       nil,
			input:       []string{"npm"},
			shouldBlock: false,
		},
		{
			name:        "block pacman with -S flag",
			cmd:         "pacman",
			args:        nil,
			flags:       []string{"-S"},
			input:       []string{"pacman", "-S", "package"},
			shouldBlock: true,
		},
		{
			name:        "allow pacman without -S flag",
			cmd:         "pacman",
			args:        nil,
			flags:       []string{"-S"},
			input:       []string{"pacman", "-Q", "package"},
			shouldBlock: false,
		},

		// `go test -exec`
		{
			name:        "go test exec",
			cmd:         "go",
			args:        []string{"test"},
			flags:       []string{"-exec"},
			input:       []string{"go", "test", "-exec", "bash -c 'echo hello'"},
			shouldBlock: true,
		},
		{
			name:        "go test exec",
			cmd:         "go",
			args:        []string{"test"},
			flags:       []string{"-exec"},
			input:       []string{"go", "test", `-exec="bash -c 'echo hello'"`},
			shouldBlock: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocker := ArgumentsBlocker(tt.cmd, tt.args, tt.flags)
			result := blocker(tt.input)
			require.Equal(t, tt.shouldBlock, result,
				"Expected block=%v for input %v", tt.shouldBlock, tt.input)
		})
	}
}

func TestCommandsBlocker(t *testing.T) {
	tests := []struct {
		name        string
		banned      []string
		input       []string
		shouldBlock bool
	}{
		{
			name:        "block single banned command",
			banned:      []string{"curl"},
			input:       []string{"curl", "https://example.com"},
			shouldBlock: true,
		},
		{
			name:        "allow non-banned command",
			banned:      []string{"curl", "wget"},
			input:       []string{"echo", "hello"},
			shouldBlock: false,
		},
		{
			name:        "block from multiple banned",
			banned:      []string{"curl", "wget", "nc"},
			input:       []string{"wget", "https://example.com"},
			shouldBlock: true,
		},
		{
			name:        "handle empty input",
			banned:      []string{"curl"},
			input:       []string{},
			shouldBlock: false,
		},
		{
			name:        "case insensitive matching on Windows",
			banned:      []string{"curl"},
			input:       []string{"CURL", "https://example.com"},
			shouldBlock: runtime.GOOS == "windows",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocker := CommandsBlocker(tt.banned)
			result := blocker(tt.input)
			require.Equal(t, tt.shouldBlock, result,
				"Expected block=%v for input %v", tt.shouldBlock, tt.input)
		})
	}
}

func TestSplitArgsFlags(t *testing.T) {
	tests := []struct {
		name      string
		input     []string
		wantArgs  []string
		wantFlags []string
	}{
		{
			name:      "only args",
			input:     []string{"install", "package", "another"},
			wantArgs:  []string{"install", "package", "another"},
			wantFlags: []string{},
		},
		{
			name:      "only flags",
			input:     []string{"-g", "--verbose", "-f"},
			wantArgs:  []string{},
			wantFlags: []string{"-g", "--verbose", "-f"},
		},
		{
			name:      "mixed args and flags",
			input:     []string{"install", "-g", "package", "--verbose"},
			wantArgs:  []string{"install", "package"},
			wantFlags: []string{"-g", "--verbose"},
		},
		{
			name:      "empty input",
			input:     []string{},
			wantArgs:  []string{},
			wantFlags: []string{},
		},
		{
			name:      "single dash flag",
			input:     []string{"-S", "package"},
			wantArgs:  []string{"package"},
			wantFlags: []string{"-S"},
		},
		{
			name:      "flag with equals sign",
			input:     []string{"-exec=bash", "package"},
			wantArgs:  []string{"package"},
			wantFlags: []string{"-exec"},
		},
		{
			name:      "long flag with equals sign",
			input:     []string{"--config=/path/to/config", "run"},
			wantArgs:  []string{"run"},
			wantFlags: []string{"--config"},
		},
		{
			name:      "flag with complex value",
			input:     []string{`-exec="bash -c 'echo hello'"`, "test"},
			wantArgs:  []string{"test"},
			wantFlags: []string{"-exec"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, flags := splitArgsFlags(tt.input)
			require.Equal(t, tt.wantArgs, args, "args mismatch")
			require.Equal(t, tt.wantFlags, flags, "flags mismatch")
		})
	}
}

func TestCanonicalExecutableCaseIsTargetPlatformSpecific(t *testing.T) {
	unix := CommandsBlockerForPlatform([]string{"curl"}, false)
	windows := CommandsBlockerForPlatform([]string{"curl"}, true)
	require.False(t, unix([]string{"CURL"}))
	require.True(t, windows([]string{"CURL.EXE"}))
	require.Equal(t, "CURL.EXE", CanonicalExecutableForPlatform("CURL.EXE", false))
	require.Equal(t, "curl", CanonicalExecutableForPlatform(`C:\\bin\\CURL.EXE`, true))
}

func TestCanonicalPowerShellEncodedCommands(t *testing.T) {
	utf16LE := func(value string) string {
		runes := utf16.Encode([]rune(value))
		raw := make([]byte, 0, len(runes)*2)
		for _, r := range runes {
			raw = append(raw, byte(r), byte(r>>8))
		}
		return base64.StdEncoding.EncodeToString(raw)
	}
	blocker := CommandsBlocker([]string{"curl"})
	for _, flag := range []string{"-EncodedCommand", "-enc", "-e"} {
		require.True(t, blocker([]string{"powershell", "-NoProfile", flag, utf16LE("curl")}), flag)
	}
	require.True(t, blocker([]string{"powershell", "-EncodedCommand", base64.StdEncoding.EncodeToString([]byte("curl"))}), "UTF-8 payload")
	require.True(t, blocker([]string{"powershell", "-EncodedCommand", "not-base64"}), "malformed payload must fail closed")
	missingCurl := "C:/missing path/curl"
	require.True(t, blocker([]string{"powershell", "-Command", "& '" + missingCurl + "'"}), "PowerShell call operator path")
	require.True(t, blocker([]string{"powershell", "-EncodedCommand", utf16LE("& '" + missingCurl + "'")}), "encoded call operator path")
	require.False(t, blocker([]string{"powershell", "-Command", "& 'echo'"}), "allowed call operator target")
	require.False(t, blocker([]string{"powershell", "-Command", "& 'C:/missing path/echo'"}), "allowed quoted path")
	for _, source := range []string{"& ('curl')", `& ("cu"+"rl")`, "& $cmd", "& 'curl", "& curl`"} {
		require.True(t, blocker([]string{"powershell", "-Command", source}), source)
		require.True(t, blocker([]string{"powershell", "-EncodedCommand", utf16LE(source)}), source)
	}
	require.False(t, blocker([]string{"powershell", "-Command", "echo curl"}), "ordinary argument")
}

func TestCanonicalArgumentBlockersUnwrapCommands(t *testing.T) {
	tests := []struct {
		name  string
		block BlockFunc
		args  []string
		want  bool
	}{
		{"absolute go install", ArgumentsBlocker("go", []string{"install"}, nil), []string{"/usr/bin/go", "install", "example.invalid/x"}, true},
		{"env npm global install", ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}), []string{"env", "npm", "install", "-g", "pkg"}, true},
		{"go test exec", ArgumentsBlocker("go", []string{"test"}, []string{"-exec"}), []string{"go", "test", "-exec", "sh"}, true},
		{"local npm install allowed", ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}), []string{"npm", "install", "pkg"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.block(tt.args))
		})
	}
}

func TestNestedCandidatesFailClosedAtDepthAndOnUnknownSyntax(t *testing.T) {
	nested := func(depth int, leaf string) []string {
		quote := func(value string) string {
			return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		}
		source := leaf
		for i := 0; i < depth; i++ {
			source = "bash -c " + quote(source)
		}
		return []string{"bash", "-c", source}
	}
	hasOpaque := func(candidates [][]string) bool {
		for _, candidate := range candidates {
			if isOpaqueCandidate(candidate) {
				return true
			}
		}
		return false
	}

	under := CommandCandidatesForPlatform(nested(7, "echo value"), true)
	require.False(t, hasOpaque(under))
	foundEcho := false
	for _, candidate := range under {
		if len(candidate) > 0 && candidate[0] == "echo" {
			foundEcho = true
			break
		}
	}
	require.True(t, foundEcho, "deepest literal leaf must remain visible")
	require.True(t, hasOpaque(CommandCandidatesForPlatform(nested(8, "echo value"), true)))
	require.True(t, hasOpaque(commandCandidates([]string{"echo"}, 9, true)))
	require.True(t, CommandsBlocker([]string{"curl"})([]string{"bash", "-c", "'unterminated"}))
	require.True(t, CommandsBlocker([]string{"curl"})([]string{"bash", "-lc", "blocked=curl; $blocked"}))

	require.False(t, CommandBlocked("blocked=echo; $blocked"))
	require.False(t, CommandBlocked("bash -lc 'echo value'"))
}
