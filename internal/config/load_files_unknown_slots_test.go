// Regression coverage for warnUnknownModelSlots: a config file still
// holding a pre-rename key under "models" (e.g. "large"/"small", from
// before the large/small -> smart/fast rename) must load without error
// but produce a loud diagnostic, since json.Unmarshal into
// map[SelectedModelType]SelectedModel accepts and then silently ignores
// any key outside the four known slots.

package config

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLoadWarnings mirrors captureDefaultSlog in startup_notices_test.go:
// warnUnknownModelSlots logs through slog.Default(), so redirecting it for
// the duration of fn is how its output is observed. Not safe alongside
// t.Parallel() for the same reason as its sibling helper.
func captureLoadWarnings(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestLoadFromBytes_WarnsOnUnrecognizedModelSlotKey(t *testing.T) {
	data := []byte(`{
		"models": {
			"large": {"model": "gpt-4o", "provider": "openai"},
			"small": {"model": "gpt-4o-mini", "provider": "openai"}
		}
	}`)

	out := captureLoadWarnings(t, func() {
		cfg, err := loadFromBytes([][]byte{data})
		require.NoError(t, err, "an unrecognized models key must not fail the load")
		require.NotNil(t, cfg)
	})

	assert.Contains(t, out, "large", "must name the offending key so the operator can fix it")
	assert.Contains(t, out, "small", "must name the offending key so the operator can fix it")
	assert.Contains(t, out, "smart, fast, worker, reviewer", "must tell the operator what the valid slots actually are")
}

func TestLoadFromBytes_NoWarningForKnownModelSlots(t *testing.T) {
	data := []byte(`{
		"models": {
			"smart": {"model": "gpt-4o", "provider": "openai"},
			"fast": {"model": "gpt-4o-mini", "provider": "openai"},
			"worker": {"model": "gpt-4o-mini", "provider": "openai"},
			"reviewer": {"model": "o1", "provider": "openai"}
		}
	}`)

	out := captureLoadWarnings(t, func() {
		_, err := loadFromBytes([][]byte{data})
		require.NoError(t, err)
	})

	assert.Empty(t, out, "all four real slot names must never trigger the unrecognized-key warning")
}

func TestLoadFromBytes_NoModelsKeyDoesNotWarn(t *testing.T) {
	data := []byte(`{"providers": {"openai": {"api_key": "key1"}}}`)

	out := captureLoadWarnings(t, func() {
		_, err := loadFromBytes([][]byte{data})
		require.NoError(t, err)
	})

	assert.Empty(t, out, "a config with no models block at all must not warn")
}
