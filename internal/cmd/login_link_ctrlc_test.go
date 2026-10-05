package cmd

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// Revert check (C9-18): without the Ctrl-C guard in oauthLinkModel.Update,
// this key press is routed into the copy branch, so the assertions below
// fail on unfixed code.
func TestOAuthLinkCtrlCDoesNotCopy(t *testing.T) {
	const url = "https://auth.openai.com/oauth2/auth?state=example"
	var copied []string
	model := &oauthLinkModel{
		url: url,
		copy: func(value string) error {
			copied = append(copied, value)
			return nil
		},
	}

	_, cmd := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.Empty(t, copied, "Ctrl-C must not copy the link")
	require.NotNil(t, cmd)
	require.IsType(t, tea.QuitMsg{}, cmd())
}

func TestOAuthLinkCtrlCInterruptsAndQuits(t *testing.T) {
	const url = "https://auth.openai.com/oauth2/auth?state=example"
	var copied []string
	interrupted := 0
	model := &oauthLinkModel{
		url: url,
		copy: func(value string) error {
			copied = append(copied, value)
			return nil
		},
		interrupt: func() { interrupted++ },
	}

	_, cmd := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.Empty(t, copied)
	require.Equal(t, 1, interrupted)
	require.NotNil(t, cmd)
	require.IsType(t, tea.QuitMsg{}, cmd())
}

func TestOAuthLinkPlainCStillCopiesWithoutInterrupting(t *testing.T) {
	const url = "https://auth.openai.com/oauth2/auth?state=example"
	var copied []string
	interrupted := 0
	model := &oauthLinkModel{
		url: url,
		copy: func(value string) error {
			copied = append(copied, value)
			return nil
		},
		interrupt: func() { interrupted++ },
	}

	_, cmd := model.Update(tea.KeyPressMsg{Code: 'c'})
	require.Equal(t, []string{url}, copied)
	require.Zero(t, interrupted)
	require.Nil(t, cmd)
}
