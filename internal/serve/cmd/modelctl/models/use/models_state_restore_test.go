package modeluse

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func restoreSel(provider, model, effort string) *config.SelectedModel {
	return &config.SelectedModel{Provider: provider, Model: model, ReasoningEffort: effort}
}

// TestRestoreCommands_Table is the pure-function table: spec shapes, scope
// suffix, ordering, empty input, and POSIX quoting.
func TestRestoreCommands_Table(t *testing.T) {
	t.Run("atom with valid effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("zai", "glm-5.3", "high"),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart glm5_3-high"}, got)
	})
	t.Run("atom without effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeFast: restoreSel("zai", "glm-5-turbo", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --fast glm5_turbo"}, got)
	})
	t.Run("atom with stale effort falls back to bare atom", func(t *testing.T) {
		// "on" is no longer a valid glm5_3_flash level (low/high/max only).
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeWorker: restoreSel("zai", "glm-5.3-flash", "on"),
		}, nil)
		assert.Equal(t, []string{"rush models use --worker glm5_3_flash"}, got)
	})
	t.Run("EffortSource atom without effort goes raw", func(t *testing.T) {
		// `models use fable5` is rejected — a level is mandatory — while the
		// raw provider/model form is accepted without one.
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("local-cli", "cli-claude-fable-5", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart local-cli/cli-claude-fable-5"}, got)
	})
	t.Run("EffortSource atom with effort keeps atom-level form", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("local-cli", "cli-claude-fable-5", "high"),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart fable5-high"}, got)
	})
	t.Run("non-atom with effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeReviewer: restoreSel("openai", "gpt-5", "high"),
		}, nil)
		assert.Equal(t, []string{"rush models use --reviewer openai/gpt-5@high"}, got)
	})
	t.Run("non-atom without effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("openai-codex", "gpt-6-luna", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart openai-codex/gpt-6-luna"}, got)
	})
	t.Run("local scope gets --local and comes after global", func(t *testing.T) {
		got := restoreCommands(
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeFast: restoreSel("zai", "glm-5-turbo", ""),
			},
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeFast: restoreSel("zai", "glm-4.7-flash", "on"),
			},
		)
		assert.Equal(t, []string{
			"rush models use --fast glm5_turbo",
			"rush models use --fast glm4_7_flash-on --local",
		}, got)
	})
	t.Run("role and scope ordering", func(t *testing.T) {
		got := restoreCommands(
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeSmart:    restoreSel("zai", "glm-5.3", "high"),
				config.SelectedModelTypeFast:     restoreSel("zai", "glm-5-turbo", ""),
				config.SelectedModelTypeWorker:   restoreSel("zai", "glm-4.7-flash", "on"),
				config.SelectedModelTypeReviewer: restoreSel("openai", "gpt-5", "max"),
			},
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeSmart: restoreSel("openai-codex", "gpt-6-luna", ""),
			},
		)
		assert.Equal(t, []string{
			"rush models use --smart glm5_3-high",
			"rush models use --smart openai-codex/gpt-6-luna --local",
			"rush models use --fast glm5_turbo",
			"rush models use --worker glm4_7_flash-on",
			"rush models use --reviewer openai/gpt-5@max",
		}, got)
	})
	t.Run("empty input is empty non-nil", func(t *testing.T) {
		got := restoreCommands(nil, nil)
		require.NotNil(t, got)
		assert.Empty(t, got)
	})
	t.Run("spec with whitespace is single-quoted", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("my prov", "mo del", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart 'my prov/mo del'"}, got)
	})
	t.Run("spec with single quote is escaped", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeFast: restoreSel("p", "o'brien", ""),
		}, nil)
		// Expected uses octal escape (backslash-quote-quote): 'p/o'\''brien'.
		assert.Equal(t, []string{"rush models use --fast 'p/o'\134''brien'"}, got)
	})
}

// TestModelsStateShowsServiceTierInEveryBlock: the fast tier appears in the EFFECTIVE and
// SCOPES rows (TIER column), in the JSON blocks and in the RESTORE command.
func TestModelsStateShowsServiceTierInEveryBlock(t *testing.T) {
	m := config.SelectedModel{Provider: "openai-codex", Model: "gpt-6.2-sol", ReasoningEffort: "high", ServiceTier: "fast"}

	eff := newStateTable("ROLE", "PROVIDER", "MODEL", "EFFORT", "TIER", "SCOPE", "ATOM", "NOTE")
	printEffectiveRow(eff, "smart", true, m, "global")
	require.Len(t, eff.rows, 1)
	assert.Equal(t, []string{"smart", "openai-codex", "gpt-6.2-sol", "high", "fast", "global"}, eff.rows[0][:6])
	assert.Equal(t, tableDash, eff.rows[0][6], "the atom cell must not suggest a command that drops the tier")

	scopes := newStateTable("SCOPE", "ROLE", "PROVIDER", "MODEL", "EFFORT", "TIER", "STATUS", "NOTE")
	printScopeRow(scopes, "global", "smart", &m, nil)
	assert.Equal(t, []string{"global", "smart", "openai-codex", "gpt-6.2-sol", "high", "fast", "effective"}, scopes.rows[0][:7])

	plain := config.SelectedModel{Provider: "openai-codex", Model: "gpt-6.2-sol", ReasoningEffort: "high"}
	printScopeRow(scopes, "global", "fast", &plain, nil)
	assert.Equal(t, tableDash, scopes.rows[1][5], "no tier renders as a dash")

	cmds := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{config.SelectedModelTypeSmart: &m}, nil)
	require.Equal(t, []string{"rush models use --smart openai-codex/gpt-6.2-sol@high/fast"}, cmds)

	selected, _, err := modelatoms.ParseAtomOrRaw("openai-codex/gpt-6.2-sol@high/fast", func(string) (string, string, bool, error) {
		return "openai-codex", "gpt-6.2-sol", true, nil
	})
	require.NoError(t, err)
	require.Equal(t, m, selected)
	require.Equal(t, "openai-codex/gpt-6.2-sol@high/fast", restoreSpec(selected))

	raw, err := json.Marshal(m)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"service_tier":"fast"`)
	raw, err = json.Marshal(plain)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "service_tier")
}
