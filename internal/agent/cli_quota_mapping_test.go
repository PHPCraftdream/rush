package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/stretchr/testify/require"
)

// Revert-check: removing mapCLIExitError in cliprovider.Stream must fail hard-wall checks; all wording here is synthetic, not captured CLI output.
func TestSyntheticCLIQuotaPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	name := "gemini"
	if runtime.GOOS == "windows" {
		name += ".cmd"
	}
	fake := filepath.Join(dir, name)
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		name, text string
		hard       bool
	}{
		{"usage reset", "Usage limit reached. Your limit will reset at 2026-06-17 14:49:28", true},
		{"quota", "quota exceeded", true},
		{"generic rate", "HTTP 429 rate limit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\nprintf '%s\\n' '" + tc.text + "' >&2\nexit 1\n"
			if runtime.GOOS == "windows" {
				script = "@echo off\r\necho " + tc.text + " 1>&2\r\nexit /b 1\r\n"
			}
			require.NoError(t, os.WriteFile(fake, []byte(script), 0o700))
			resolved, err := exec.LookPath("gemini")
			require.NoError(t, err)
			require.Equal(t, fake, resolved, "must resolve only the isolated fake CLI")
			p := cliprovider.New(t.TempDir(), t.TempDir(), func() bool { return true }, nil, nil, nil)
			model, err := p.LanguageModel(context.Background(), "cli-gemini-pro")
			require.NoError(t, err)
			_, err = model.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("synthetic test")}})
			require.Error(t, err)
			require.Equal(t, classTerminal, classifyProviderError(err))
			require.Equal(t, tc.hard, IsHardQuotaLimit(err))
			require.False(t, isRateLimitError(err), "generic rate text remains generic terminal, not retryable")
			var exit *exec.ExitError
			require.True(t, errors.As(err, &exit))
			if tc.name == "usage reset" {
				reset, ok := QuotaLimitResetTime(err, time.Now())
				require.True(t, ok)
				require.Equal(t, 2026, reset.Year())
			}
		})
	}
}
