package server

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	for _, test := range []struct {
		host string
		want bool
	}{
		{"", false},
		{"::", false},
		{"[::1]", true},
		{"::ffff:127.0.0.1", true},
		{"localhost.evil.com", false},
		{"LOCALHOST", true},
		{"0.0.0.0", false},
		{"192.168.1.5", false},
	} {
		t.Run(test.host, func(t *testing.T) {
			if got := IsLoopbackHost(test.host); got != test.want {
				t.Fatalf("IsLoopbackHost(%q) = %t, want %t", test.host, got, test.want)
			}
		})
	}
}
