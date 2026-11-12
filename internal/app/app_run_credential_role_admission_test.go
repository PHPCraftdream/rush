package app

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

const admissionCustomRole = "Research Analyst"

func admissionCredentials(customSlot string, fallback bool) *agent.CredentialSet {
	models := map[agent.Role]agent.ModelChoice{
		agent.RoleSmart: {Provider: "tenant", Model: "tenant-smart"},
		agent.RoleFast:  {Provider: "tenant", Model: "tenant-fast"},
	}
	if customSlot != "" {
		models[agent.Role(customSlot)] = agent.ModelChoice{Provider: "tenant", Model: "tenant-custom"}
	}
	return &agent.CredentialSet{
		Credentials: []agent.Credential{{
			Provider: "tenant",
			Type:     agent.ProviderTypeOpenAICompat,
			APIKey:   "tenant-key",
			BaseURL:  "https://tenant.invalid/v1",
		}},
		Models:                      models,
		AllowConfiguredRoleFallback: fallback,
	}
}

func prepareCustomRoleAdmission(t *testing.T, app *App, creds *agent.CredentialSet, role string) (*runExecSetup, error) {
	t.Helper()
	_, cancel, setup, err := app.prepareExecuteRun(t.Context(), RunRequest{
		Prompt:      "p",
		HideSpinner: true,
		Credentials: creds,
		Overrides:   RunOverrides{RequestedRole: role},
	})
	if cancel != nil {
		cancel()
	}
	return setup, err
}

func TestPrepareExecuteRunRejectsUncoveredCustomCredentialRole(t *testing.T) {
	for name, customSlot := range map[string]string{
		"missing":        "",
		"wrong case":     strings.ToLower(admissionCustomRole),
		"wrong spelling": "ResearchAnalyst",
	} {
		t.Run(name, func(t *testing.T) {
			app := newModelEffortApp(t)
			require.NoError(t, app.config.SetCustomRole(config.ScopeWorkspace, admissionCustomRole, config.CustomRoleUpdate{
				Model: config.SelectedModel{Provider: "openaicompat", Model: "configured-custom"},
			}))
			before, err := app.Sessions.ListAll(t.Context())
			require.NoError(t, err)

			setup, err := prepareCustomRoleAdmission(t, app, admissionCredentials(customSlot, false), admissionCustomRole)
			require.EqualError(t, err, `credential set does not cover requested custom role "Research Analyst"`)
			require.Nil(t, setup)

			after, err := app.Sessions.ListAll(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after, "rejected custom-role credentials must not create a session")
		})
	}
}

func TestPrepareExecuteRunCustomCredentialRoleAdmissionExceptions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		customSlot string
		fallback   bool
		requested  string
	}{
		{name: "trusted configured fallback", fallback: true, requested: admissionCustomRole},
		{name: "exact tenant custom slot", customSlot: admissionCustomRole, requested: admissionCustomRole},
		{name: "built-in role remains independent", requested: string(config.SelectedModelTypeSmart)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newModelEffortApp(t)
			require.NoError(t, app.config.SetCustomRole(config.ScopeWorkspace, admissionCustomRole, config.CustomRoleUpdate{
				Model: config.SelectedModel{Provider: "openaicompat", Model: "configured-custom"},
			}))
			overrides := RunOverrides{RequestedRole: tc.requested}
			if tc.name == "built-in role remains independent" {
				overrides = RunOverrides{ModelRole: config.SelectedModelTypeSmart}
			}
			_, cancel, setup, err := app.prepareExecuteRun(t.Context(), RunRequest{
				Prompt: "p", HideSpinner: true, Credentials: admissionCredentials(tc.customSlot, tc.fallback), Overrides: overrides,
			})
			require.NoError(t, err)
			t.Cleanup(cancel)
			require.NotNil(t, setup)
			require.NotNil(t, setup.credsRunner)
			if tc.requested == admissionCustomRole {
				require.NotNil(t, setup.callOpts.RoleContract)
				require.Equal(t, admissionCustomRole, setup.callOpts.RoleContract.Name)
			} else {
				require.Nil(t, setup.callOpts.RoleContract, "built-in execution must not bind a custom role contract")
				require.Equal(t, config.SelectedModelTypeSmart, setup.callOpts.ModelRole)
			}
		})
	}
}

func TestPrepareExecuteRunMalformedCredentialsPrecedeCustomRoleCoverage(t *testing.T) {
	app := newModelEffortApp(t)
	require.NoError(t, app.config.SetCustomRole(config.ScopeWorkspace, admissionCustomRole, config.CustomRoleUpdate{
		Model: config.SelectedModel{Provider: "openaicompat", Model: "configured-custom"},
	}))
	creds := admissionCredentials("", false)
	creds.Credentials = nil

	setup, err := prepareCustomRoleAdmission(t, app, creds, admissionCustomRole)
	require.EqualError(t, err, "invalid per-call credentials: credential set has no credentials")
	require.Nil(t, setup)
}
