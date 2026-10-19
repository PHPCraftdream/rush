package agentguard

import (
	"strings"
	"testing"
)

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
