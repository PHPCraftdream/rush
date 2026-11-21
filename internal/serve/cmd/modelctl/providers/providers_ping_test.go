package providers

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestDefaultBaseURLFor(t *testing.T) {
	cases := []struct {
		typ  string
		want string
	}{
		{"anthropic", "https://api.anthropic.com/v1"},
		{"Anthropic", "https://api.anthropic.com/v1"}, // case-insensitive
		{"gemini", "https://generativelanguage.googleapis.com/v1beta"},
		{"openai", "https://api.openai.com/v1"},
		{"openai-compat", "https://api.openai.com/v1"}, // fallback
		{"random-thing", "https://api.openai.com/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			got := defaultBaseURLFor(config.ProviderConfig{Type: catwalk.Type(tc.typ)})
			assert.Equal(t, tc.want, got)
		})
	}
}
