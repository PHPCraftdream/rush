package modelkit

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactNetworkURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "userinfo stripped scheme and host kept",
			in:   `unsupported proxy scheme: "http://user:secret@proxy.example.com:8080"`,
			want: `unsupported proxy scheme: "http://proxy.example.com:8080"`,
		},
		{
			name: "query dropped",
			in:   "DoH lookup failed for https://dns.example.com/query?token=s3cret&x=1 endpoint",
			want: "DoH lookup failed for https://dns.example.com/query endpoint",
		},
		{
			name: "userinfo and query together keep path",
			in:   "dial failed via http://alice:hunter2@10.0.0.1:9/path/here?api-key=k",
			want: "dial failed via http://10.0.0.1:9/path/here",
		},
		{
			name: "trailing sentence punctuation survives redaction",
			in:   "connection refused (http://user:secret@proxy.example.com:8080).",
			want: "connection refused (http://proxy.example.com:8080).",
		},
		{
			name: "ipv6 host kept",
			in:   "connect http://user:pw@[::1]:8080/x?q=1 refused",
			want: "connect http://[::1]:8080/x refused",
		},
		{
			name: "text without urls untouched",
			in:   "plain failure with no url",
			want: "plain failure with no url",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, RedactNetworkURLs(tc.in))
		})
	}
}

func TestRedactNetworkErrorPreservesUnwrap(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("underlying: http://user:secret@host:1/x?token=k")
	wrapped := redactNetworkError(sentinel)

	require.ErrorIs(t, wrapped, sentinel)
	require.NotContains(t, wrapped.Error(), "user:secret")
	require.NotContains(t, wrapped.Error(), "token=k")
	require.Contains(t, wrapped.Error(), "http://host:1/x")
}
