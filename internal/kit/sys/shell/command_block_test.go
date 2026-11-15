package shell

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shpolicy"
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
				shpolicy.ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
			},
			command:     "npm install -g typescript",
			shouldBlock: true,
		},
		{
			name: "block npm global install with --global",
			blockFuncs: []BlockFunc{
				shpolicy.ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
			},
			command:     "npm install --global typescript",
			shouldBlock: true,
		},
		{
			name: "allow npm local install",
			blockFuncs: []BlockFunc{
				shpolicy.ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
				shpolicy.ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
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

func TestCanonicalBlockersUnwrapCommands(t *testing.T) {
	commandBlocker := shpolicy.CommandsBlocker([]string{"curl"})
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

func TestCanonicalRuntimeBlockerSeesExpandedAndNestedCommands(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	missingCurl := filepath.ToSlash(filepath.Join(dir, "curl"))
	require.True(t, shpolicy.CommandBlocked("bash -lc 'curl > "+sentinel+"'"))
	for _, command := range []string{
		"blocked='" + missingCurl + "'; $blocked",
		"bash -c 'curl'",
		"bash -lc 'curl'",
		"sh -xc 'curl'",
		"powershell -EncodedCommand YwB1AHIAbAA=",
	} {
		_, _, err := NewShell(&Options{
			WorkingDir: dir,
			BlockFuncs: shpolicy.CanonicalBlockFuncs(),
		}).Exec(t.Context(), command)
		require.Error(t, err, command)
	}
	require.NoFileExists(t, sentinel)
}
