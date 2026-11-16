package cliout

import "testing"

func TestIsServeInvocationCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"serve"}, true},
		{[]string{"serve", "--cwd", "/tmp"}, true},
		{[]string{"serve", "--unknown"}, true},
		{[]string{"serve", "unexpected"}, true},
		{[]string{"tokens", "list"}, false},
		{[]string{"unknown"}, false},
		{nil, false},
	} {
		if got := isServeInvocation(tc.args); got != tc.want {
			t.Errorf("isServeInvocation(%q)=%t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestTokensInvocationIsCommandScoped(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"tokens", "list"}, true},
		{[]string{"run", "tokens"}, false},
		{[]string{"--name", "tokens", "run"}, false},
		{[]string{"--", "tokens"}, false},
	} {
		if got := isTokensInvocation(tc.args); got != tc.want {
			t.Errorf("isTokensInvocation(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
