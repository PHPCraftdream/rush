package sdk_test

// Strict CredentialSet isolation tests (review-round-1 finding R1-5):
// a CredentialSet that does not cover a smart/fast role must fail
// CLOSED — rejected by validation, or hard-errored by the coordinator —
// BEFORE any provider traffic, so tenant data can never leak onto the
// operator's configured provider through a missing or misspelled role.
// The operator's provider below is a live httptest server: every
// fail-closed test asserts it received zero requests, and the one
// fallback test asserts the AllowConfiguredRoleFallback crossing
// actually lands on the operator's provider, API key and all.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// strictHarness wires the operator-vs-tenant provider split used by
// the run-level tests in this file: the operator's provider comes from
// a rush.json in the workspace (config smart/fast), the tenant's from
// a CredentialSet pointing at a different httptest server.
type strictHarness struct {
	client   *sdk.Client
	operator *credentialServer
	tenant   *credentialServer
	workDir  string
}

func newStrictHarness(t *testing.T) *strictHarness {
	t.Helper()
	// Same global-config isolation as
	// TestRunWithCredentialsConcurrentTenantsIsolated: config.Init
	// inside sdk.Open must read only the rush.json written below.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	h := &strictHarness{
		operator: newCredentialServer(t, "OPERATOR_PROVIDER_MUST_NOT_BE_HIT"),
		tenant:   newCredentialServer(t, "TENANT_PROVIDER_OK"),
		workDir:  t.TempDir(),
	}
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "operator": {
      "id": "operator",
      "name": "operator",
      "type": "openai-compat",
      "base_url": %q,
      "api_key": "operator-key",
      "discover_models": false,
      "models": [
        {"id": "operator-model", "name": "operator-model", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "operator", "model": "operator-model"},
    "fast": {"provider": "operator", "model": "operator-model"}
  }
}`, h.operator.srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(h.workDir, "rush.json"), []byte(rushJSON), 0o644))

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: h.workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	h.client = client
	return h
}

// strictCreds builds a CredentialSet over the harness's tenant server
// with exactly the given role coverage.
func strictCreds(h *strictHarness, models map[sdk.Role]sdk.ModelChoice, allowFallback bool) sdk.CredentialSet {
	return sdk.CredentialSet{
		Credentials: []sdk.Credential{
			{
				Provider: "tenant-provider",
				Type:     sdk.ProviderTypeOpenAICompat,
				APIKey:   "sk-tenant-strict-secret",
				BaseURL:  h.tenant.srv.URL,
				Models: []sdk.CredentialModel{
					{ID: "tenant-model", ContextWindow: 200000, DefaultMaxTokens: 1000},
				},
			},
		},
		Models:                      models,
		AllowConfiguredRoleFallback: allowFallback,
	}
}

// offlineCreds is strictCreds without a live server, for pure
// Validate() unit tests.
func offlineCreds(models map[sdk.Role]sdk.ModelChoice) sdk.CredentialSet {
	return sdk.CredentialSet{
		Credentials: []sdk.Credential{
			{
				Provider: "tenant-provider",
				Type:     sdk.ProviderTypeOpenAICompat,
				APIKey:   "sk-offline",
				BaseURL:  "https://tenant.example/v1",
				Models: []sdk.CredentialModel{
					{ID: "tenant-model", ContextWindow: 200000, DefaultMaxTokens: 1000},
				},
			},
		},
		Models: models,
	}
}

// validStrictModels is the minimal compliant coverage: smart + fast.
func validStrictModels() map[sdk.Role]sdk.ModelChoice {
	return map[sdk.Role]sdk.ModelChoice{
		sdk.RoleSmart: {Provider: "tenant-provider", Model: "tenant-model"},
		sdk.RoleFast:  {Provider: "tenant-provider", Model: "tenant-model"},
	}
}

// runStrictTurn performs one RunWithCredentials turn on a fresh
// session id (get-or-create, no seeding, so background title
// generation fires on whatever the fast role resolves to — that is
// exactly the traffic the fail-closed tests must not see on the
// operator).
func runStrictTurn(t *testing.T, h *strictHarness, sessionID string, creds sdk.CredentialSet) (*sdk.RunResult, error) {
	t.Helper()
	var buf bytes.Buffer
	res, err := h.client.RunWithCredentials(context.Background(), sdk.RunRequest{
		Prompt:            "reply with exactly the marker text and nothing else",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
	}, creds)
	if err == nil && res != nil && res.ExitReason != "end_turn" {
		return res, fmt.Errorf("exit_reason=%q error=%q warnings=%v output=%q", res.ExitReason, res.Error, res.Warnings, buf.String())
	}
	return res, err
}

func TestCredentialSetValidateRejectsUnknownRole(t *testing.T) {
	models := validStrictModels()
	models[sdk.Role("smrat")] = sdk.ModelChoice{Provider: "tenant-provider", Model: "tenant-model"}
	cs := offlineCreds(models)
	err := cs.Validate()
	require.ErrorContains(t, err, "unknown role")
}

func TestCredentialSetValidateRejectsUnknownProviderType(t *testing.T) {
	cs := offlineCreds(validStrictModels())
	cs.Credentials[0].Type = sdk.ProviderType("myapi")
	err := cs.Validate()
	require.ErrorContains(t, err, "unknown type")
}

func TestCredentialSetValidateRequiresSmartRole(t *testing.T) {
	missingSmart := offlineCreds(map[sdk.Role]sdk.ModelChoice{
		sdk.RoleFast: {Provider: "tenant-provider", Model: "tenant-model"},
	})
	err := missingSmart.Validate()
	require.ErrorContains(t, err, "must define the smart role")

	// The minimal compliant set needs only the smart role: Validate
	// does NOT require fast (library mode documents fast as optional);
	// the fast-role strictness lives in the coordinator's resolve path.
	onlySmart := offlineCreds(map[sdk.Role]sdk.ModelChoice{
		sdk.RoleSmart: {Provider: "tenant-provider", Model: "tenant-model"},
	})
	require.NoError(t, onlySmart.Validate())
}

func TestRunWithCredentialsTypoRoleFailsBeforeAnyTraffic(t *testing.T) {
	h := newStrictHarness(t)
	models := validStrictModels()
	models[sdk.Role("smrat")] = sdk.ModelChoice{Provider: "tenant-provider", Model: "tenant-model"}

	_, err := runStrictTurn(t, h, "sdk-strict-typo", strictCreds(h, models, false))
	require.ErrorContains(t, err, "unknown role")
	require.Equal(t, 0, h.operator.totalRequests(),
		"a misspelled role must fail validation before any provider traffic")
	require.Equal(t, 0, h.tenant.totalRequests())
}

func TestRunWithCredentialsMissingSmartFailsBeforeAnyTraffic(t *testing.T) {
	h := newStrictHarness(t)
	models := map[sdk.Role]sdk.ModelChoice{
		sdk.RoleFast:   {Provider: "tenant-provider", Model: "tenant-model"},
		sdk.RoleWorker: {Provider: "tenant-provider", Model: "tenant-model"},
	}

	_, err := runStrictTurn(t, h, "sdk-strict-no-smart", strictCreds(h, models, false))
	require.ErrorContains(t, err, "must define the smart role")
	require.Equal(t, 0, h.operator.totalRequests(),
		"a missing smart role must fail validation before any provider traffic")
	require.Equal(t, 0, h.tenant.totalRequests())
}

func TestRunWithCredentialsMissingFastFailsClosedByDefault(t *testing.T) {
	h := newStrictHarness(t)
	models := map[sdk.Role]sdk.ModelChoice{
		sdk.RoleSmart:  {Provider: "tenant-provider", Model: "tenant-model"},
		sdk.RoleWorker: {Provider: "tenant-provider", Model: "tenant-model"},
	}

	// No AllowConfiguredRoleFallback: the old behavior silently served
	// title generation (the fast role) from the operator's configured
	// model. It must now be a hard error with zero provider traffic.
	_, err := runStrictTurn(t, h, "sdk-strict-no-fast", strictCreds(h, models, false))
	require.ErrorContains(t, err, "does not cover the fast role")
	require.Equal(t, 0, h.operator.totalRequests(),
		"a missing fast role must NOT fall back to the operator's configured provider")
	require.Equal(t, 0, h.tenant.totalRequests())
}

func TestRunWithCredentialsMissingFastFallbackCrossesBoundary(t *testing.T) {
	h := newStrictHarness(t)
	models := map[sdk.Role]sdk.ModelChoice{
		sdk.RoleSmart: {Provider: "tenant-provider", Model: "tenant-model"},
	}

	// AllowConfiguredRoleFallback=true is the documented escape hatch:
	// title generation (the uncovered fast role) deliberately rides the
	// OPERATOR's configured provider and API key, while the main turn
	// still runs on the tenant's credentials.
	res, err := runStrictTurn(t, h, "sdk-strict-fallback", strictCreds(h, models, true))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "TENANT_PROVIDER_OK", res.FinalText,
		"the main turn must still be served by the tenant's provider")

	require.GreaterOrEqual(t, h.operator.totalRequests(), 1,
		"AllowConfiguredRoleFallback=true must actually serve the uncovered fast role from the operator's configured model")
	for auth := range h.operator.hits() {
		require.Equal(t, "Bearer operator-key", auth,
			"fallback title generation must use the OPERATOR's API key, proving the documented boundary crossing")
	}
	for auth := range h.tenant.hits() {
		require.Equal(t, "Bearer sk-tenant-strict-secret", auth,
			"the tenant's provider must only ever see the tenant's API key")
	}
}

func TestRunWithCredentialsWorkerAndReviewerOptionalStayIsolated(t *testing.T) {
	h := newStrictHarness(t)
	// worker and reviewer legitimately absent: sub-agent spawns would
	// fall back to the tenant's smart model INSIDE the credential set,
	// never to config. The turn (including background title generation
	// on the covered fast role) must stay entirely on the tenant.
	res, err := runStrictTurn(t, h, "sdk-strict-no-worker", strictCreds(h, validStrictModels(), false))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "TENANT_PROVIDER_OK", res.FinalText)
	require.GreaterOrEqual(t, h.tenant.totalRequests(), 1,
		"the tenant's provider must serve the turn and its title generation")
	require.Equal(t, 0, h.operator.totalRequests(),
		"absent worker/reviewer roles must not send anything to the operator's provider")
}
