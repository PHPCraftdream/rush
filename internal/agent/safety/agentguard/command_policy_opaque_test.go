package agentguard

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPolicyTransparentPrefixOptionVerdicts(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	denied := [][]string{
		{"time", "-f", "%e", "go", "test"},
		{"bash", "-c", "exec -a ignored go test ./..."},
		{"time", "-p", "go", "test"},
		{"exec", "-a", "x", "go", "build"},
		{"nice", "-n", "5", "go", "vet", "./..."},
		{"env", "-i", "FOO=1", "go", "test"},
		{"timeout", "5", "go", "test"},
		{"sudo", "-u", "bob", "go", "test"},
		{"command", "-p", "go", "test"},
		{"xargs", "-n1", "go", "build"},
		{"env", "-S", "go test", "git", "status"},
		{"env", "-S", "go test", "-i"},
		{"env", "-u", "X", "-S", "npm run build", "echo"},
		{"nice", "env", "-S", "cargo build", "true"},
		{"bash", "-c", `env -S "go vet ./..." true`},
	}
	for _, argv := range denied {
		if err := p.CheckArgs(argv); err == nil {
			t.Errorf("CheckArgs(%q) allowed", argv)
		}
	}
	allowed := [][]string{
		{"time", "-f", "%e", "git", "status"},
		{"exec", "-a", "x", "git", "log"},
		{"nice", "-n", "5", "gofmt", "-l", "."},
		{"env", "-S", "git status", "true"},
		{"env", "-i", "git", "log"},
	}
	for _, argv := range allowed {
		if err := p.CheckArgs(argv); err != nil {
			t.Errorf("CheckArgs(%q) = %v", argv, err)
		}
	}
}

func TestOpaqueShellWrapperPolicy(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	cases := []struct {
		argv         []string
		rule, reason string
	}{
		{[]string{"bash", "-c", "git status"}, "", ""},
		{[]string{"bash", "-c", "cd x && git status"}, "", ""},
		{[]string{"sh", "-c", "go test ./..."}, "go test", ""},
		{[]string{"bash", "-c", "$tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"bash", "-c", "${tool} test"}, "opaque-wrapper-command", "${tool}"},
		{[]string{"bash", "-c", "$(echo go) test"}, "opaque-wrapper-command", "$(echo go)"},
		{[]string{"bash", "-c", "`echo go` test"}, "opaque-wrapper-command", "`echo go`"},
		{[]string{"bash", "-c", "eval $cmd"}, "opaque-wrapper-command", "eval"},
		{[]string{"bash", "-c", "source $script"}, "opaque-wrapper-command", "source"},
		{[]string{"bash", "-c", ". ./script"}, "opaque-wrapper-command", "."},
		{[]string{"bash", "-c", "tool=go; \"$tool\" test ./..."}, "opaque-wrapper-command", "$tool"},
		{[]string{"bash", "-c", "\"${tools[@]}\" build"}, "opaque-wrapper-command", "${tools[@]}"},
		{[]string{"bash", "-c", "'go' test"}, "go test", ""},
		{[]string{"bash", "--noprofile", "-c", "$tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"bash", "-lc", "$tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"bash", "-c", "git status", "go test"}, "", ""},
		{[]string{"powershell", "-Command", "git status"}, "", ""},
		{[]string{"powershell", "-Command", "& $tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"powershell", "-ExecutionPolicy", "Bypass", "-Command", "& $tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"powershell", "-Command", ".\t$tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"pwsh", "-c", "Write-Output ok; iex $cmd"}, "opaque-wrapper-command", "iex"},
		{[]string{"cmd", "/c", "%GO% test"}, "opaque-wrapper-command", "%GO%"},
		{[]string{"env", "X=1", "bash", "-c", "$tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"xargs", "bash", "-c", "$tool test"}, "opaque-wrapper-command", "$tool"},
		{[]string{"sh", ".githooks/check_file_size.sh"}, "", ""},
		{[]string{"bash", ".githooks/check_file_size.sh"}, "", ""},
	}
	for _, tc := range cases {
		err := p.CheckArgs(tc.argv)
		if tc.rule == "" {
			if err != nil {
				t.Errorf("CheckArgs(%q) = %v", tc.argv, err)
			}
			continue
		}
		denied, ok := err.(*CommandDeniedError)
		if !ok || denied.Rule != tc.rule {
			t.Errorf("CheckArgs(%q) = %#v, want rule %s", tc.argv, err, tc.rule)
			continue
		}
		if tc.reason != "" && !strings.Contains(denied.Detail, tc.reason) {
			t.Errorf("CheckArgs(%q) detail %q missing %q", tc.argv, denied.Detail, tc.reason)
		}
		if tc.rule == "opaque-wrapper-command" && (!strings.Contains(denied.Error(), "under RUSH_DENY_EXEC dynamic commands are refused because tests/builds run only in GitHub CI") || !strings.Contains(denied.Error(), "literal allowed command")) {
			t.Errorf("opaque explanation %q", denied.Error())
		}
	}
}

func TestOpaqueShellWrapperPolicyDisabled(t *testing.T) {
	p := NewCommandPolicy("")
	for _, argv := range [][]string{{"bash", "-c", "$tool test"}, {"powershell", "-Command", "& $tool test"}, {"cmd", "/c", "%GO% test"}, {"bash", "-c", "$(echo go) test"}} {
		if err := p.CheckArgs(argv); err != nil {
			t.Errorf("disabled CheckArgs(%q) = %v", argv, err)
		}
	}
}

func TestOpaquePolicyTransparentPrefixesAndDynamicActions(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	opaque := [][]string{{"bash", "-c", "command eval x"}, {"bash", "-c", "builtin source f"}, {"bash", "-c", "env X=1 eval x"}, {"bash", "-c", "nohup eval x"}, {"bash", "-c", "timeout 2 command eval x"}, {"bash", "-c", "capm 2g eval x"}, {"bash", "-c", `\eval x`}, {"bash", "-c", `'eval' x`}}
	for _, argv := range opaque {
		err := p.CheckArgs(argv)
		d, ok := err.(*CommandDeniedError)
		if !ok || d.Rule != "opaque-wrapper-command" {
			t.Errorf("CheckArgs(%q)=%v; want opaque denial", argv, err)
		}
	}
	allowed := [][]string{{"bash", "-c", `git status "$path"`}, {"bash", "-c", `grep -r "$pat" .`}, {"bash", "-c", `cd "$d" && gofmt -l .`}}
	for _, argv := range allowed {
		if err := p.CheckArgs(argv); err != nil {
			t.Errorf("CheckArgs(%q)=%v", argv, err)
		}
	}
	for _, payload := range []string{"go $s ./...", "npm run $s", "bun run $s", "npx run $s", "cargo $c", "go -C \"$d\" test", "$t test"} {
		err := p.CheckArgs([]string{"bash", "-c", payload})
		if err == nil {
			t.Errorf("CheckArgs(%q) allowed", payload)
		}
	}
	if err := p.CheckArgs([]string{"bash", "-c", policyDynamicArg}); err == nil {
		t.Error("CheckArgs allowed a dynamic -c payload")
	}
	if err := NewCommandPolicy("").CheckArgs([]string{"bash", "-c", "eval x"}); err != nil {
		t.Fatalf("disabled: %v", err)
	}
}

func TestOpaquePolicyCheckCommandCases(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	cases := []struct {
		command string
		denied  bool
	}{
		{"bash -c 'env X=1 command builtin exec nohup time nice eval $code'", true},
		{"bash -c '\\eval $code'", true},
		{"bash -c \"'env' X=1 'command' 'source' $script\"", true},
		{"bash -c 'go $s ./...'", true},
		{"bash -c 'npm run $s'", true},
		{"bash -c 'cargo $c'", true},
		{"bash -c 'go -C \"$d\" test'", true},
		{"bash -c '$t test'", true},
		{"bash -c 'git status \"$path\"'", false},
		{"bash -c 'grep -r \"$pat\" .'", false},
		{"bash -c 'cd \"$d\" && gofmt -l .'", false},
	}
	for _, tc := range cases {
		err := p.CheckCommand(tc.command)
		if (err != nil) != tc.denied {
			t.Errorf("CheckCommand(%q) = %v, denied=%v", tc.command, err, tc.denied)
		}
	}
	if err := NewCommandPolicy("").CheckCommand("bash -c '$tool test'"); err != nil {
		t.Fatalf("disabled policy: %v", err)
	}
}

func TestPowerShellLiteralPrefixControls(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	for _, argv := range [][]string{{"pwsh", "-Command", "Start-Process Invoke-Expression"}, {"pwsh", "-Command", "call iex 'go test'"}} {
		err := p.CheckArgs(argv)
		d, ok := err.(*CommandDeniedError)
		if !ok || d.Rule != "opaque-wrapper-command" {
			t.Errorf("CheckArgs(%q)=%v; want opaque", argv, err)
		}
	}
	if err := p.CheckArgs([]string{"pwsh", "-Command", "Start-Process git"}); err != nil {
		t.Errorf("literal allowed Start-Process: %v", err)
	}
}

func TestCommandDeniedErrorDetail(t *testing.T) {
	err := NewCommandPolicy("ci-only").CheckArgs([]string{"bash", "-c", "$tool test"})
	denied, ok := err.(*CommandDeniedError)
	if !ok || denied.Rule != "opaque-wrapper-command" || denied.Detail == "" {
		t.Fatalf("opaque denial = %#v", err)
	}
	if !strings.Contains(denied.Error(), "matched rule opaque-wrapper-command") {
		t.Fatalf("error message = %q", denied.Error())
	}
}

func TestCheck_AllowsRushLockInspectionAndRun(t *testing.T) {
	commands := []string{"rush lock list", "rush lock list --cwd /tmp/work", "rush lock list --data-dir /tmp/rush-data", "rush lock run task-138 -- go test ./internal/agent/safety/agentguard", "rush lock run task-138 --cwd /tmp/work --data-dir /tmp/data -- echo ok", "/usr/bin/rush lock list", "rush.exe lock run task-138 -- echo ok", "echo ok && rush lock list"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) { require.NoError(t, Check(command)) })
	}
	argvs := [][]string{{"rush", "lock", "list"}, {"rush", "lock", "list", "--cwd", "/tmp/work"}, {"rush", "lock", "list", "--data-dir", "/tmp/data"}, {"echo", "ok", ";", "echo", "still-ok"}, {"rush", "lock", "run", "task-138", "--", "go", "test", "./internal/agent/safety/agentguard"}, {"rush", "lock", "run", "task-138", "--cwd", "/tmp/work", "--data-dir", "/tmp/data", "--", "echo", "ok"}}
	for _, argv := range argvs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) { require.NoError(t, CheckArgs(argv)) })
	}
}

func TestCheck_AllowsRushLockRunOptions(t *testing.T) {
	commands := []string{
		"rush lock run task --timeout 10m --wait 2m --idle 1m --on-stuck=release --force-safe -- echo ok",
		"rush lock run task --on-stuck stop --timeout=10m -- echo ok",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) { require.NoError(t, Check(command)) })
	}
	for _, argv := range [][]string{
		{"rush", "lock", "run", "task", "--timeout", "10m", "--wait", "2m", "--idle", "1m", "--on-stuck=release", "--force-safe", "--", "echo", "ok"},
		{"rush", "lock", "run", "task", "--on-stuck", "stop", "--timeout=10m", "--", "echo", "ok"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) { require.NoError(t, CheckArgs(argv)) })
	}
}

func TestCommandPolicyRushLockCandidates(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	direct := []string{"go", "test", "./..."}
	compositions := [][]string{
		{"env", "rush", "lock", "run", "x", "--", "go", "test", "./..."},
		{"rush", "lock", "run", "x", "--", "env", "go", "test", "./..."},
		{"nice", "env", "rush", "lock", "run", "x", "--", "rush", "lock", "run", "y", "--", "go", "test", "./..."},
		{"rush", "lock", "run", "x", "--", "bash", "-c", "go test ./..."},
	}
	directDenied := p.CheckArgs(direct) != nil
	require.True(t, directDenied)
	for _, wrapped := range compositions {
		candidates := shell.CommandCandidatesForPlatform(wrapped, true)
		found := false
		for _, candidate := range candidates {
			if len(candidate) >= 2 && candidate[0] == "go" && candidate[1] == "test" {
				found = true
			}
		}
		require.True(t, found, "wrapped go test must be a command candidate: %q", candidates)
		require.Equal(t, directDenied, p.CheckArgs(wrapped) != nil, "wrapped/direct policy parity for %q", wrapped)
	}
	require.NoError(t, p.CheckArgs([]string{"rush", "lock", "run", "job", "--", "echo", "ok"}))

	for _, nested := range [][]string{{"rush", "run", "task"}, {"rush", "sessions"}} {
		argv := append([]string{"rush", "lock", "run", "job", "--"}, nested...)
		require.Error(t, CheckArgs(argv), "%q", argv)
	}
	require.Error(t, Check("rush lock run job -- rush sessions"))
}

func TestCheck_DeniesUnexpectedRushLockRunOptions(t *testing.T) {
	for _, command := range []string{
		"rush lock run task --unknown -- echo ok",
		"rush lock run task --timeout -- echo ok",
		"rush lock --yes run task -- echo ok",
	} {
		t.Run(command, func(t *testing.T) { require.Error(t, Check(command)) })
	}
	for _, argv := range [][]string{
		{"rush", "lock", "run", "task", "--unknown", "--", "echo", "ok"},
		{"rush", "lock", "run", "task", "--timeout", "--", "echo", "ok"},
		{"rush", "lock", "--yes", "run", "task", "--", "echo", "ok"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) { require.Error(t, CheckArgs(argv)) })
	}
}

func TestCheck_DeniesOtherRushFormsAndWrappedInnerAgents(t *testing.T) {
	commands := []string{"rush lock", "rush lock task", "rush lock --yes run task -- echo ok", "rush unlock", "rush run task", "rush sessions", "crush lock list", "rush lock list --yes", "rush lock run task echo ok", "rush lock run task --", "rush lock run task -- claude", "rush lock run task -- codex", "rush lock run task -- gemini", "rush lock run task -- rush run task", "rush lock run task -- bash -c claude", "rush lock list --cwd /tmp/work --yes", "rush lock run task --cwd /tmp/work --yes -- echo ok", "/usr/bin/rush lock --yes", "rush.exe run task", "claude"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			err := Check(command)
			require.Error(t, err)
			var denied *DeniedError
			require.ErrorAs(t, err, &denied)
			if strings.Contains(command, "crush") {
				assert.Equal(t, "crush", denied.Tool)
			} else if !strings.Contains(command, "claude") && !strings.Contains(command, "codex") && !strings.Contains(command, "gemini") {
				assert.Equal(t, "rush", denied.Tool)
			}
		})
	}
	argvs := [][]string{{"rush", "lock"}, {"rush", "lock", "word"}, {"rush", "lock", "--yes", "run", "task", "--", "echo"}, {"rush", "unlock"}, {"rush", "run", "task"}, {"rush", "sessions"}, {"crush", "lock", "list"}, {"rush", "lock", "list", "--yes"}, {"rush", "lock", "run", "task", "echo"}, {"rush", "lock", "run", "task", "--"}, {"rush", "lock", "run", "task", "--", "claude"}, {"rush", "lock", "run", "task", "--", "codex"}, {"rush", "lock", "run", "task", "--", "gemini"}, {"rush", "lock", "run", "task", "--", "rush", "run", "task"}, {"rush", "lock", "run", "task", "--", "rush", "sessions"}, {"bash", "-c", "rush lock run task -- claude"}, {"bash", "-c", "rush lock list --yes"}, {"/usr/bin/rush", "lock", "--yes"}, {"rush.exe", "run", "task"}, {policyDynamicArg, "lock", "list"}, {"rush", "lock", policyDynamicArg}, {"rush", "lock", "list", ";", "claude"}, {"rush", "lock", "list", "&&", "codex"}}
	for _, argv := range argvs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) { require.Error(t, CheckArgs(argv)) })
	}
}

func TestCheck_DeniesNestedAgentsInRushLockCompositions(t *testing.T) {
	for _, argv := range [][]string{
		{"env", "rush", "lock", "run", "x", "--", "claude"},
		{"rush", "lock", "run", "x", "--", "env", "claude"},
		{"nice", "env", "rush", "lock", "run", "x", "--", "rush", "lock", "run", "y", "--", "claude"},
		{"rush", "lock", "run", "x", "--", "bash", "-c", "claude"},
	} {
		require.Error(t, CheckArgs(argv), "%q", argv)
	}
	for _, command := range []string{
		"env rush lock run x -- claude",
		"rush lock run x -- env claude",
		"nice env rush lock run x -- rush lock run y -- claude",
		"rush lock run x -- bash -c 'claude'",
	} {
		require.Error(t, Check(command), "%s", command)
	}
}

func TestCheck_DeniesDynamicRushLockHead(t *testing.T) {
	for _, command := range []string{"$TOOL lock list", "${TOOL} lock list"} {
		t.Run(command, func(t *testing.T) {
			require.Error(t, Check(command))
		})
	}
	for _, args := range [][]string{
		{policyDynamicArg, "lock", "list"},
		{"$TOOL", "lock", "list"},
		{"${TOOL}", "lock", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.Error(t, CheckArgs(args))
		})
	}
}

func TestCheck_ChainedLockSegments(t *testing.T) {
	cases := []struct{ name, cmd, want string }{{"allows harmless chain ending in lock list", "echo ok && rush lock list", ""}, {"denies agent after allowed lock list", "rush lock list; claude", "claude"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.cmd)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var denied *DeniedError
			require.ErrorAs(t, err, &denied)
			assert.Equal(t, tc.want, denied.Tool)
		})
	}
}

func TestCheck_RejectsRushWithShellOperatorSuffix(t *testing.T) {
	err := Check("rush lock list; claude")
	require.Error(t, err)
	var denied *DeniedError
	require.ErrorAs(t, err, &denied)
	assert.Equal(t, "claude", denied.Tool)
}

func TestCheckArgs_OperatorsAreArgumentData(t *testing.T) {
	// Structured argv does not interpret shell metacharacters as separators.
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{name: "printf semicolon is data", argv: []string{"printf", ";", "claude"}},
		{name: "rush list trailing argument is invalid", argv: []string{"rush", "lock", "list", ";"}, want: "rush"},
		{name: "lock run payload operator is data", argv: []string{"rush", "lock", "run", "task", "--", "echo ; claude"}},
		{name: "wrapped printf semicolon is a separate data element", argv: []string{"rush", "lock", "run", "task", "--", "printf", ";", "claude"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckArgs(tc.argv)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "trailing extra argument invalidates allowed lock list form, not chain rejection")
			var denied *DeniedError
			require.ErrorAs(t, err, &denied)
			assert.Equal(t, tc.want, denied.Tool, "trailing extra argument invalidates allowed lock list form, not chain rejection")
		})
	}
}

func TestCommandBlockFuncAllowsAndBlocksRushLockForms(t *testing.T) {
	block := CommandBlockFunc()
	assert.False(t, block([]string{"rush", "lock", "list"}))
	assert.False(t, block([]string{"/usr/bin/rush", "lock", "list"}))
	assert.False(t, block([]string{"rush.exe", "lock", "run", "task", "--", "echo", "ok"}))
	assert.True(t, block([]string{"rush", "lock", "run", "task", "--", "claude"}))
	assert.Error(t, CheckArgs([]string{policyDynamicArg, "lock", "list"}))
	assert.True(t, block([]string{policyDynamicArg, "lock", "list"}))
	assert.Error(t, CheckArgs([]string{"rush", "lock", "list", ";", "echo", "ok", ";", "claude"}))
}

func TestCheckArgs_LockWrapperWithUninspectableInnerFailsClosed(t *testing.T) {
	chain := func(depth int, leaf ...string) []string {
		var argv []string
		for i := 0; i < depth; i++ {
			argv = append(argv, "rush", "lock", "run", string(rune('a'+i)), "--")
		}
		return append(argv, leaf...)
	}
	// A short chain with a harmless leaf is inspected completely and allowed.
	require.NoError(t, CheckArgs(chain(3, "printf", "ok")))
	// Past the expansion depth limit the leaf cannot be inspected: deny even
	// though every visible outer form is valid and the leaf itself is harmless.
	deep := CheckArgs(chain(9, "printf", "ok"))
	require.Error(t, deep, "depth exhaustion must not be allowed on the strength of the outer lock form")
	var denied *DeniedError
	require.ErrorAs(t, deep, &denied)
	assert.Equal(t, "rush", denied.Tool)
	// A denied agent at the leaf of a short chain is denied for the agent.
	visible := CheckArgs(chain(3, "claude"))
	require.ErrorAs(t, visible, &denied)
	assert.Equal(t, "claude", denied.Tool)
}
