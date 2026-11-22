package install

// Moved with validateClaudeInitSources (its subject) from the targets
// conversion test file; fstest.MapFS stands in for the embedded sources.

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
)

func TestValidateClaudeInitSourcesReturnsContextualYAMLErrors(t *testing.T) {
	valid := []byte("description: x\nblocks:\n  - common: body\n")
	tests := []struct {
		name     string
		files    fstest.MapFS
		contains string
	}{
		{
			name: "rush source",
			files: fstest.MapFS{
				"claude_slash_command.yaml":          {Data: []byte("description: x\nunknown: y\nblocks:\n  - common: body\n")},
				"claude_crush_fallback_command.yaml": {Data: valid},
			},
			contains: "rush slash-command source",
		},
		{
			name: "fallback source",
			files: fstest.MapFS{
				"claude_slash_command.yaml":          {Data: valid},
				"claude_crush_fallback_command.yaml": {Data: []byte("description: x\nunknown: y\nblocks:\n  - common: body\n")},
			},
			contains: "rush-fallback slash-command source",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateClaudeInitSources(test.files)
			assert.ErrorContains(t, err, test.contains)
			assert.ErrorContains(t, err, "unknown")
		})
	}
}
