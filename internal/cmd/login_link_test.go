package cmd

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func TestOAuthLinkControlsRequireExplicitKeyPress(t *testing.T) {
	const url = "https://auth.openai.com/oauth/authorize?state=example"
	var opened, copied []string
	model := &oauthLinkModel{
		url: url,
		open: func(value string) error {
			opened = append(opened, value)
			return nil
		},
		copy: func(value string) error {
			copied = append(copied, value)
			return nil
		},
	}

	view := model.View().Content
	require.Less(t, strings.Index(view, url), strings.Index(view, "[o] Open link"))
	require.Contains(t, view, "[c] Copy full link")
	require.Empty(t, opened)
	require.Empty(t, copied)

	model.Update(tea.KeyPressMsg{Code: 'x'})
	require.Empty(t, opened)
	require.Empty(t, copied)
	model.Update(tea.KeyPressMsg{Code: 'c'})
	require.Equal(t, []string{url}, copied)
	require.Empty(t, opened)
	model.Update(tea.KeyPressMsg{Code: 'o'})
	require.Equal(t, []string{url}, opened)
}

func TestOAuthLinkViewDoesNotTruncateLongURL(t *testing.T) {
	const url = "https://auth.openai.com/oauth/authorize?client_id=codex&code_challenge=long-value&state=some-state"
	model := &oauthLinkModel{url: url}
	model.Update(tea.WindowSizeMsg{Width: 30})

	view := model.View().Content
	lines := strings.Split(view, "\n")
	var renderedURL strings.Builder
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		require.LessOrEqual(t, len(line), 29)
		renderedURL.WriteString(line)
	}
	require.Equal(t, url, renderedURL.String())
	require.Greater(t, strings.Index(view, "\n\n[o] Open link"), strings.Index(view, "https://"))
}

func TestOAuthLinkControlsShowActionFailures(t *testing.T) {
	model := &oauthLinkModel{
		url:  "https://example.com/authorize",
		open: func(string) error { return errors.New("browser unavailable") },
		copy: func(string) error { return errors.New("clipboard unavailable") },
	}
	model.Update(tea.KeyPressMsg{Code: 'o'})
	require.Contains(t, model.View().Content, "browser unavailable")
	model.Update(tea.KeyPressMsg{Code: 'c'})
	require.Contains(t, model.View().Content, "clipboard unavailable")
}
