package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMatchRoleToolPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern string
		name    string
		want    bool
	}{
		{pattern: "", name: "", want: true},
		{pattern: "", name: "tool"},
		{pattern: "smart", name: "smart", want: true},
		{pattern: "tool", name: "tool-extra"},
		{pattern: "Tool", name: "tool"},
		{pattern: "tool", name: "Tool"},
		{pattern: "mcp_*", name: "mcp_search", want: true},
		{pattern: "mcp_*", name: "mcp_", want: true},
		{pattern: "mcp_*", name: "other_mcp_search"},
		{pattern: "read?", name: "readX", want: true},
		{pattern: "read?", name: "read界", want: true},
		{pattern: "?", name: "界", want: true},
		{pattern: "?", name: "e\u0301"},
		{pattern: "?", name: ""},
		{pattern: "*", name: "", want: true},
		{pattern: "**", name: "anything", want: true},
		{pattern: "a*b?c", name: "ab界c", want: true},
		{pattern: "a*b?c", name: "a/b界c", want: true},
		{pattern: "a*b?c", name: "ab界cd"},
		{pattern: "mcp.server-name/tool[search]", name: "mcp.server-name/tool[search]", want: true},
		{pattern: `mcp.server-name/tool[?]`, name: `mcp.server-name/tool[x]`, want: true},
		{pattern: `mcp.server-name/tool[?]`, name: `mcp.server-name/tool[xy]`},
		{pattern: `mcp\server\tool`, name: `mcp\server\tool`, want: true},
		{pattern: `mcp\server\*`, name: `mcp\server\tool`, want: true},
		{pattern: `mcp/server/*`, name: `mcp/server/search`, want: true},
		{pattern: `mcp/server/*`, name: `mcp\server\search`},
		{pattern: `[a-z]+(tool).`, name: `[a-z]+(tool).`, want: true},
		{pattern: `[a-z]+(tool).`, name: `abc-toolX`},
		{pattern: `mcp__server__search`, name: `mcp__server__search`, want: true},
	} {
		t.Run(tc.pattern+" / "+tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, MatchRoleToolPattern(tc.pattern, tc.name))
		})
	}
}

func TestMatchRoleToolPatternExhaustiveShortPatterns(t *testing.T) {
	t.Parallel()
	patterns := shortStrings([]rune{'a', 'b', '?', '*'}, 4)
	names := shortStrings([]rune{'a', 'b'}, 4)
	for _, pattern := range patterns {
		for _, name := range names {
			require.Equalf(t, referenceRoleToolMatch([]rune(pattern), []rune(name)), MatchRoleToolPattern(pattern, name), "pattern %q, name %q", pattern, name)
		}
	}
}

func TestRoleToolsAllow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		patterns []string
		tool     string
		want     bool
	}{
		{name: "empty list denies", tool: "view"},
		{name: "no match", patterns: []string{"edit", "mcp_*"}, tool: "view"},
		{name: "later pattern matches", patterns: []string{"edit", "mcp_*"}, tool: "mcp_search", want: true},
		{name: "exact match", patterns: []string{"view"}, tool: "view", want: true},
		{name: "full name required", patterns: []string{"view"}, tool: "view_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, RoleToolsAllow(tc.patterns, tc.tool))
		})
	}
}

func shortStrings(alphabet []rune, maxLength int) []string {
	values := []string{""}
	frontier := []string{""}
	for length := 1; length <= maxLength; length++ {
		next := make([]string, 0, len(frontier)*len(alphabet))
		for _, prefix := range frontier {
			for _, char := range alphabet {
				value := prefix + string(char)
				next = append(next, value)
				values = append(values, value)
			}
		}
		frontier = next
	}
	return values
}

func referenceRoleToolMatch(pattern, name []rune) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	switch pattern[0] {
	case '*':
		for consumed := 0; consumed <= len(name); consumed++ {
			if referenceRoleToolMatch(pattern[1:], name[consumed:]) {
				return true
			}
		}
		return false
	case '?':
		return len(name) > 0 && referenceRoleToolMatch(pattern[1:], name[1:])
	default:
		return len(name) > 0 && pattern[0] == name[0] && referenceRoleToolMatch(pattern[1:], name[1:])
	}
}
