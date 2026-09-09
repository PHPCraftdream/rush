package shell

import (
	"encoding/base64"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func TestCommandBlocking(t *testing.T) {
	tests := []struct {
		name        string
		blockFuncs  []BlockFunc
		command     string
		shouldBlock bool
	}{
		{
			name: "block simple command",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) > 0 && args[0] == "curl"
				},
			},
			command:     "curl https://example.com",
			shouldBlock: true,
		},
		{
			name: "allow non-blocked command",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) > 0 && args[0] == "curl"
				},
			},
			command:     "echo hello",
			shouldBlock: false,
		},
		{
			name: "block subcommand",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) >= 2 && args[0] == "brew" && args[1] == "install"
				},
			},
			command:     "brew install wget",
			shouldBlock: true,
		},
		{
			name: "allow different subcommand",
			blockFuncs: []BlockFunc{
				func(args []string) bool {
					return len(args) >= 2 && args[0] == "brew" && args[1] == "install"
				},
			},
			command:     "brew list",
			shouldBlock: false,
		},
		{
			name: "block npm global install with -g",
			blockFuncs: []BlockFunc{
				ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
			},
			command:     "npm install -g typescript",
			shouldBlock: true,
		},
		{
			name: "block npm global install with --global",
			blockFuncs: []BlockFunc{
				ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
			},
			command:     "npm install --global typescript",
			shouldBlock: true,
		},
		{
			name: "allow npm local install",
			blockFuncs: []BlockFunc{
				ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
				ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
			},
			command:     "npm install typescript",
			shouldBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a temporary directory for each test
			tmpDir := t.TempDir()

			shell := NewShell(&Options{
				WorkingDir: tmpDir,
				// Run every case with PATH pointed at an empty directory.
				// The blocking decision happens before the interpreter ever
				// looks a binary up, so this does not weaken what is under
				// test: the assertions below only ever check whether the
				// security error was returned, and the allowed cases are
				// explicitly expected to be free to fail for any other
				// reason (see the comment on the else branch below).
				//
				// Without this, the shouldBlock:false rows ran for real
				// against whatever happened to be installed on the machine
				// — and "allow npm local install" performed an actual
				// `npm install typescript`, downloading the package into
				// t.TempDir(). Measured at 34.2s of this package's 39.3s
				// total `go test -short` runtime, plus a hard dependency on
				// npm and on outbound network access from every CI runner
				// in the matrix.
				Env:        []string{"PATH=" + t.TempDir()},
				BlockFuncs: tt.blockFuncs,
			})

			_, _, err := shell.Exec(t.Context(), tt.command)

			if tt.shouldBlock {
				if err == nil {
					t.Errorf("Expected command to be blocked, but it was allowed")
				} else if !strings.Contains(err.Error(), "not allowed for security reasons") {
					t.Errorf("Expected security error, got: %v", err)
				}
			} else {
				// For non-blocked commands, we might get other errors (like command not found)
				// but we shouldn't get the security error
				if err != nil && strings.Contains(err.Error(), "not allowed for security reasons") {
					t.Errorf("Command was unexpectedly blocked: %v", err)
				}
			}
		})
	}
}

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

func TestCanonicalBlockersUnwrapCommands(t *testing.T) {
	commandBlocker := CommandsBlocker([]string{"curl"})
	for _, test := range []struct {
		name string
		args []string
	}{
		{"absolute path", []string{"/usr/bin/curl"}},
		{"windows path", []string{`C:\\Tools\\curl.exe`}},
		{"env", []string{"env", "curl"}},
		{"env assignment", []string{"env", "FOO=bar", "curl"}},
		{"env split string", []string{"env", "-S", "curl"}},
		{"nice", []string{"nice", "-n", "10", "curl"}},
		{"timeout", []string{"timeout", "--kill-after", "2", "30", "curl"}},
		{"timeout equals options", []string{"timeout", "--kill-after=2", "--signal=TERM", "30", "curl"}},
		{"command", []string{"command", "curl"}},
		{"exec", []string{"exec", "curl"}},
		{"nohup", []string{"nohup", "curl"}},
		{"time", []string{"time", "curl"}},
		{"time format", []string{"time", "-f", "%e", "curl"}},
		{"time long format", []string{"time", "--format", "%e", "curl"}},
		{"time output", []string{"time", "-o", "timing.txt", "curl"}},
		{"time long output", []string{"time", "--output=timing.txt", "curl"}},
		{"posix shell", []string{"bash", "-c", "curl example.invalid"}},
		{"posix shell cluster", []string{"bash", "-lc", "curl example.invalid"}},
		{"posix shell x cluster", []string{"sh", "-xc", "curl example.invalid"}},
		{"posix shell long option", []string{"bash", "--rcfile", "profile", "-lc", "curl example.invalid"}},
		{"cmd shell", []string{"cmd.exe", "/c", "curl example.invalid"}},
		{"cmd shell flags", []string{"cmd.exe", "/d", "/c", "curl example.invalid"}},
		{"powershell", []string{"powershell.exe", "-Command", "curl example.invalid"}},
		{"powershell flags", []string{"powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", "curl example.invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !commandBlocker(test.args) {
				t.Fatalf("command was not blocked: %q", test.args)
			}
		})
	}

	for _, command := range []string{"mycurl example.invalid", "echo curl", "echo /tmp/curl.txt"} {
		args, ok := ParseSimpleCommand(command)
		require.True(t, ok)
		require.False(t, commandBlocker(args), command)
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

func TestCanonicalRuntimeBlockerSeesExpandedAndNestedCommands(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	missingCurl := filepath.ToSlash(filepath.Join(dir, "curl"))
	require.True(t, CommandBlocked("bash -lc 'curl > "+sentinel+"'"))
	for _, command := range []string{
		"blocked='" + missingCurl + "'; $blocked",
		"bash -c 'curl'",
		"bash -lc 'curl'",
		"sh -xc 'curl'",
		"powershell -EncodedCommand YwB1AHIAbAA=",
	} {
		_, _, err := NewShell(&Options{
			WorkingDir: dir,
			BlockFuncs: CanonicalBlockFuncs(),
		}).Exec(t.Context(), command)
		require.Error(t, err, command)
	}
	require.NoFileExists(t, sentinel)
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
