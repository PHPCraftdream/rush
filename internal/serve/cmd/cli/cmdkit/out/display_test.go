package cliout

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMaskKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "-"},
		{"abcd", "****"},
		{"abc", "****"},
		{"sk-1234567890", "****7890"},
		{"$OPENAI_API_KEY", "$OPENAI_API_KEY"}, // unresolved env template: pass through
		{"${VAR:-default}", "${VAR:-default}"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, MaskKey(tc.in))
		})
	}
}

func TestDash(t *testing.T) {
	assert.Equal(t, "-", Dash(""))
	assert.Equal(t, "x", Dash("x"))
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "hello", Truncate("hello", 10))
	assert.Equal(t, "hello", Truncate("hello", 5))
	assert.Equal(t, "hell…", Truncate("hello world", 5))
	assert.Equal(t, "h", Truncate("hello", 1))
}

func TestShort(t *testing.T) {
	assert.Equal(t, "", Short(""))
	assert.Equal(t, "abc", Short("abc"))
	assert.Equal(t, "01234567", Short("0123456789abcdef"))
}
