package agentguard

import (
	"strings"
	"testing"
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
