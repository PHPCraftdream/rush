// peak_hours window parsing tests: the parsePeakHoursWindow table
// (normal/overnight/boundary windows, clearing forms, error cases), its
// delegation to config.PeakHoursWindow.Validate, and the `providers set
// --peak-hours` / `--peak-hours-message` CLI's behaviour: setting the
// message alongside a window, message-only updates, clearing, scoping and
// the "message requires a window" guard.
package cmd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePeakHoursWindow(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNil   bool
		wantStart string
		wantEnd   string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "normal window",
			input:     "09:00-18:00",
			wantNil:   false,
			wantStart: "09:00",
			wantEnd:   "18:00",
		},
		{
			name:      "overnight window",
			input:     "22:00-06:00",
			wantNil:   false,
			wantStart: "22:00",
			wantEnd:   "06:00",
		},
		{
			name:      "boundary start end of day",
			input:     "00:00-23:59",
			wantNil:   false,
			wantStart: "00:00",
			wantEnd:   "23:59",
		},
		{
			name:    "empty clears (nil)",
			input:   "",
			wantNil: true,
		},
		{
			name:    "off clears (nil)",
			input:   "off",
			wantNil: true,
		},
		{
			name:    "OFF clears case-insensitive",
			input:   "OFF",
			wantNil: true,
		},
		{
			name:    "whitespace-only clears (nil)",
			input:   "   ",
			wantNil: true,
		},
		{
			name:      "trims whitespace",
			input:     "  09:00 - 18:00  ",
			wantNil:   false,
			wantStart: "09:00",
			wantEnd:   "18:00",
		},
		{
			name:      "missing dash",
			input:     "09:00",
			wantErr:   true,
			errSubstr: "expected HH:MM-HH:MM",
		},
		{
			name:      "missing end time",
			input:     "09:00-",
			wantErr:   true,
			errSubstr: "both start and end must be set",
		},
		{
			name:      "bad start format no leading zero",
			input:     "9:00-18:00",
			wantErr:   true,
			errSubstr: "peak_hours start",
		},
		{
			name:      "bad end format dash instead of colon",
			input:     "09:00-18-00",
			wantErr:   true,
			errSubstr: "peak_hours end",
		},
		{
			name:      "hour out of range",
			input:     "24:00-18:00",
			wantErr:   true,
			errSubstr: "peak_hours start",
		},
		{
			name:      "minute out of range",
			input:     "09:00-18:60",
			wantErr:   true,
			errSubstr: "peak_hours end",
		},
		{
			name:      "garbage input",
			input:     "nope",
			wantErr:   true,
			errSubstr: "expected HH:MM-HH:MM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := parsePeakHoursWindow(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errSubstr != "" {
					assert.Contains(t, err.Error(), tt.errSubstr)
				}
				assert.Nil(t, w)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, w)
				return
			}
			require.NotNil(t, w)
			assert.Equal(t, tt.wantStart, w.Start)
			assert.Equal(t, tt.wantEnd, w.End)
		})
	}
}

func TestParsePeakHoursWindow_ReusesConfigValidate(t *testing.T) {
	// The parser must delegate to PeakHoursWindow.Validate, so a window
	// that Validate rejects must also be rejected here.
	w := config.PeakHoursWindow{Start: "09:00", End: ""}
	require.Error(t, w.Validate())

	_, err := parsePeakHoursWindow("09:00-")
	require.Error(t, err)
}

// TestProvidersSet_PeakHoursMessagePreservedOnTimeOnlyUpdate is a
// regression for the CLI's own documented contract ("Only the flags you
// pass are written — unset fields are left untouched"): --peak-hours only
// carries HH:MM-HH:MM, so it must not drop a message that was already
// configured — whether that message came from an earlier
// --peak-hours-message run or from the web UI.
func TestProvidersSet_PeakHoursMessagePreservedOnTimeOnlyUpdate(t *testing.T) {
	seedJSON := `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00", "message": "Ping #ops-oncall first."}
    }
  }
}`
	_, globalDataPath := runProvidersCmdInIsolatedApp(t, providersSetCmd, seedJSON, "with-peak --peak-hours 10:00-20:00")

	raw, err := os.ReadFile(globalDataPath)
	require.NoError(t, err)

	var out struct {
		Providers map[string]struct {
			PeakHours *config.PeakHoursWindow `json:"peak_hours"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))

	p, ok := out.Providers["with-peak"]
	require.True(t, ok, "provider must still be present after the update")
	require.NotNil(t, p.PeakHours, "peak_hours must not be cleared by a time-only update")
	assert.Equal(t, "10:00", p.PeakHours.Start, "the time window must actually update")
	assert.Equal(t, "20:00", p.PeakHours.End)
	assert.Equal(t, "Ping #ops-oncall first.", p.PeakHours.Message,
		"the message configured through the web UI must survive a CLI-side time-only --peak-hours update")
}

// A provider WITHOUT any peak_hours — the starting point for the tests
// that add a window/message via the CLI.
const seedNoPeakHoursJSON = `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}]
    }
  }
}`

// A provider WITH a full peak_hours object (start, end, message).
const seedPeakHoursWithMessageJSON = `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00", "message": "old-note"}
    }
  }
}`

// A provider WITH a peak_hours window but no message yet.
const seedPeakHoursNoMessageJSON = `{
  "providers": {
    "with-peak": {
      "name": "With Peak",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}],
      "peak_hours": {"start": "09:00", "end": "18:00"}
    }
  }
}`

// readPersistedProviders decodes the persisted rush.json at path into the
// same anonymous-struct shape used by the preservation test above.
func readPersistedProviders(t *testing.T, path string) map[string]struct {
	PeakHours *config.PeakHoursWindow `json:"peak_hours"`
	Disable   bool                    `json:"disable"`
} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var out struct {
		Providers map[string]struct {
			PeakHours *config.PeakHoursWindow `json:"peak_hours"`
			Disable   bool                    `json:"disable"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out.Providers
}

func TestProvidersSet_PeakHoursMessageWithWindow(t *testing.T) {
	// Regression for --peak-hours-message on `providers set`: the flag was
	// added alongside --peak-hours, so a window+message write must persist
	// BOTH parts of peak_hours.
	_, globalPath := runProvidersCmdInIsolatedApp(t, providersSetCmd, seedNoPeakHoursJSON,
		"with-peak --peak-hours 09:00-18:00 --peak-hours-message=call-oncall-first")

	providers := readPersistedProviders(t, globalPath)
	p, ok := providers["with-peak"]
	require.True(t, ok, "provider must still be present after the update")
	require.NotNil(t, p.PeakHours, "peak_hours must be written")
	assert.Equal(t, "09:00", p.PeakHours.Start)
	assert.Equal(t, "18:00", p.PeakHours.End)
	assert.Equal(t, "call-oncall-first", p.PeakHours.Message)
}

func TestProvidersSet_PeakHoursMessageOnly_PreservesWindow(t *testing.T) {
	// A message-only update must keep the existing window (start/end) and
	// only replace the message — the persisted peak_hours stays a complete,
	// valid object rather than a bare message fragment.
	_, globalPath := runProvidersCmdInIsolatedApp(t, providersSetCmd, seedPeakHoursNoMessageJSON,
		"with-peak --peak-hours-message=updated-note")

	providers := readPersistedProviders(t, globalPath)
	p, ok := providers["with-peak"]
	require.True(t, ok, "provider must still be present after the update")
	require.NotNil(t, p.PeakHours, "peak_hours must survive a message-only update")
	assert.Equal(t, "09:00", p.PeakHours.Start, "the window must be preserved")
	assert.Equal(t, "18:00", p.PeakHours.End, "the window must be preserved")
	assert.Equal(t, "updated-note", p.PeakHours.Message)
}

func TestProvidersSet_PeakHoursMessageClearedWithEmptyValue(t *testing.T) {
	// `--peak-hours-message=` (empty value, but the flag IS changed) clears
	// the message while keeping the window — and because message is
	// omitempty, the key must be gone from the file entirely.
	_, globalPath := runProvidersCmdInIsolatedApp(t, providersSetCmd, seedPeakHoursWithMessageJSON,
		"with-peak --peak-hours-message=")

	providers := readPersistedProviders(t, globalPath)
	p, ok := providers["with-peak"]
	require.True(t, ok, "provider must still be present after the update")
	require.NotNil(t, p.PeakHours, "peak_hours must survive clearing the message")
	assert.Equal(t, "09:00", p.PeakHours.Start, "the window must be preserved")
	assert.Equal(t, "18:00", p.PeakHours.End, "the window must be preserved")
	assert.Equal(t, "", p.PeakHours.Message)

	raw, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "old-note",
		"the dropped message must not linger in the persisted bytes (message is omitempty)")
}

func TestProvidersSet_PeakHoursMessage_RequiresWindow(t *testing.T) {
	// A message with no window to attach it to is a hard error: there is
	// nothing to append the message to, and a bare message fragment would
	// be an invalid peak_hours object. Nothing must be persisted.
	_, globalPath, _, runErr := runProvidersCmdInIsolatedAppFull(t, providersSetCmd, seedNoPeakHoursJSON,
		"with-peak --peak-hours-message=orphan-note", "")

	require.Error(t, runErr, "a message without a --peak-hours window must fail")
	assert.Contains(t, runErr.Error(), "no peak-hours window")

	providers := readPersistedProviders(t, globalPath)
	p, ok := providers["with-peak"]
	require.True(t, ok, "provider must still be present")
	assert.Nil(t, p.PeakHours, "no peak_hours may be written when the command fails")
}

func TestProvidersSet_PeakHoursOff_WinsOverMessage(t *testing.T) {
	// `--peak-hours off` clears the whole window (and therefore the
	// message with it); a simultaneously passed --peak-hours-message is
	// ignored rather than resurrecting a window-less message.
	_, globalPath := runProvidersCmdInIsolatedApp(t, providersSetCmd, seedPeakHoursWithMessageJSON,
		"with-peak --peak-hours off --peak-hours-message=ignored")

	providers := readPersistedProviders(t, globalPath)
	p, ok := providers["with-peak"]
	require.True(t, ok, "provider must still be present after clearing peak hours")
	assert.Nil(t, p.PeakHours, "--peak-hours off must clear both the window and the message")
}

func TestProvidersSet_PeakHoursMessage_LocalScope(t *testing.T) {
	// --local must write a COMPLETE, valid peak_hours object into the
	// workspace file (the effective window carried across, with the new
	// message) and leave the global file untouched.
	_, globalPath, workspacePath, runErr := runProvidersCmdInIsolatedAppFull(t, providersSetCmd, seedPeakHoursNoMessageJSON,
		"with-peak --local --peak-hours-message=workspace-only-msg", ".rush")
	require.NoError(t, runErr, "command RunE failed")

	require.NotEmpty(t, workspacePath, "the helper must return the workspace rush.json path")

	wsProviders := readPersistedProviders(t, workspacePath)
	wsP, ok := wsProviders["with-peak"]
	require.True(t, ok, "provider must be written into the workspace file")
	require.NotNil(t, wsP.PeakHours, "the workspace file must hold a complete peak_hours object")
	assert.Equal(t, "09:00", wsP.PeakHours.Start, "the window must be carried into the workspace file")
	assert.Equal(t, "18:00", wsP.PeakHours.End)
	assert.Equal(t, "workspace-only-msg", wsP.PeakHours.Message)

	globalProviders := readPersistedProviders(t, globalPath)
	gP, ok := globalProviders["with-peak"]
	require.True(t, ok, "the global provider must still be present")
	require.NotNil(t, gP.PeakHours, "the global peak_hours must be untouched")
	assert.Equal(t, "", gP.PeakHours.Message, "--local must not touch the global message")

	wsRaw, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	globalRaw, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Contains(t, string(wsRaw), "workspace-only-msg")
	assert.NotContains(t, string(globalRaw), "workspace-only-msg")
}

func TestProvidersAdd_PeakHoursMessage(t *testing.T) {
	// `providers add` accepts the window and its message in one shot.
	// --enable=false keeps the command offline (no model fetch) and there is
	// no --api-key, so no connection test is attempted either.
	seedJSON := `{"providers":{}}`
	_, globalPath := runProvidersCmdInIsolatedApp(t, providersAddCmd, seedJSON,
		"newprov --name New --type openai --enable=false --peak-hours 22:00-06:00 --peak-hours-message=overnight-note")

	providers := readPersistedProviders(t, globalPath)
	p, ok := providers["newprov"]
	require.True(t, ok, "provider must be persisted")
	require.NotNil(t, p.PeakHours, "peak_hours must be persisted")
	assert.Equal(t, "22:00", p.PeakHours.Start, "an overnight window must round-trip")
	assert.Equal(t, "06:00", p.PeakHours.End)
	assert.Equal(t, "overnight-note", p.PeakHours.Message)
	assert.True(t, p.Disable, "--enable=false must persist disable=true")
}

func TestProvidersAdd_PeakHoursMessage_RequiresWindow(t *testing.T) {
	// Same guard as `providers set`: the message is meaningless without a
	// window, so add must refuse and persist nothing.
	seedJSON := `{"providers":{}}`
	_, globalPath, _, runErr := runProvidersCmdInIsolatedAppFull(t, providersAddCmd, seedJSON,
		"newprov --name New --type openai --enable=false --peak-hours-message=orphan", "")

	require.Error(t, runErr, "--peak-hours-message without --peak-hours must fail")
	assert.Contains(t, runErr.Error(), "--peak-hours")

	providers := readPersistedProviders(t, globalPath)
	_, ok := providers["newprov"]
	assert.False(t, ok, "no provider may be persisted when the command fails")
}
