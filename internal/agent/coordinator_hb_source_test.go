package agent

// Regression tests for the heartbeat source-label plumbing: the labels
// decided where a slot was resolved must ride the resolvedOverrides
// snapshot and be attached to the turn ctx in runInternal, not lost on a
// local ctx inside the resolve functions.
//
// REVERT-CHECKS (each test must fail on exactly the listed single change):
//
//	TestCoordinatorResolve_SessionOverrideSourceLabels — delete the
//	  resolved.smartSource/fastSource assignment in
//	  resolveSessionModelsInternal: the labels regress to empty.
//	TestRunInternal_SourceReachesProviderCtx — delete the
//	  `ctx = withModelSource(ctx, pinned.smartSource, pinned.fastSource)`
//	  attach in runInternal: the provider ctx source regresses to
//	  slot@start.
import (
	"context"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestCoordinatorResolve_SessionOverrideSourceLabels pins that a session
// row override is reported as hbSourceSessionOverride on the snapshot.
func TestCoordinatorResolve_SessionOverrideSourceLabels(t *testing.T) {
	hbIsolateEnv(t)
	coord, _ := newCallOptionsTestCoordinator(t)

	sess, err := coord.sessions.Create(t.Context(), "hb-src-session")
	require.NoError(t, err)
	err = coord.sessions.UpdateModels(t.Context(), sess.ID, &session.ModelSlotUpdate{
		Provider: "test-peak",
		Model:    "test-model",
	}, nil)
	require.NoError(t, err)

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, hbSourceSessionOverride, pinned.smartSource)
	require.Equal(t, hbSourceSlotStart, pinned.fastSource)
}

// hbSourceCaptureAgent wraps a SessionAgent and records the ctx each Run
// receives, so tests can assert the source label that reached the turn.
type hbSourceCaptureAgent struct {
	SessionAgent
	mu  sync.Mutex
	ctx context.Context
}

func (a *hbSourceCaptureAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	return a.SessionAgent.Run(ctx, call)
}

// TestRunInternal_SourceReachesProviderCtx pins that runInternal attaches
// the snapshot's source labels onto the ctx handed to the provider.
func TestRunInternal_SourceReachesProviderCtx(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)

	providerCfg := config.ProviderConfig{
		ID:   "hb-prov",
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "hb-model", Name: "HB Model", DefaultMaxTokens: 4096},
		},
	}
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set("hb-prov", providerCfg)

	smart := hbModel("hb-prov", "hb-smart", fantasy.Usage{})
	fast := hbModel("hb-prov", "hb-fast", fantasy.Usage{})
	capture := &hbSourceCaptureAgent{SessionAgent: hbAgent(env, smart, fast)}

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	coord.currentAgent = capture

	sess, err := coord.sessions.Create(t.Context(), "hb-src-turn")
	require.NoError(t, err)

	pinned := &resolvedOverrides{
		smart:       smart,
		fast:        fast,
		smartSource: hbSourceSessionOverride,
		providerCfg: providerCfg,
	}
	ctx := WithCallOptions(t.Context(), &CallOptions{AllowPeakHours: true})
	_, err = coord.runInternal(ctx, sess.ID, "hello", pinned)
	require.NoError(t, err)

	require.GreaterOrEqual(t, smart.Model.(*hbCtxModel).calls(), 1)
	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, sess.ID, "smart", hbSourceSessionOverride, heartbeat.PurposeTurn)
}

// TestApplyModelOverrides_PerCallSourceLabels: an explicit per-call model
// is labelled per-call, an untouched slot slot@start.
// Revert check: hbOverrideSource instead of hbCallSource (or no assignment)
// in applyModelOverrides.
func TestApplyModelOverrides_PerCallSourceLabels(t *testing.T) {
	hbIsolateEnv(t)
	coord, _ := newCallOptionsTestCoordinator(t)
	pinned, err := coord.applyModelOverrides(t.Context(), &ModelOverride{Provider: "test-peak", Model: "test-model"}, nil)
	require.NoError(t, err)
	require.Equal(t, hbSourcePerCall, pinned.smartSource)
	require.Equal(t, hbSourceSlotStart, pinned.fastSource)
}

// TestResolveCredentialsModels_SourceLabels: credential-set models are
// per-call; a fast slot served from the configured fallback keeps the
// fallback's label.
// Revert check: dropping the smartSource/fastSource assignments after
// buildCredentialModel, or the base label copy, in resolveCredentialsModels.
func TestResolveCredentialsModels_SourceLabels(t *testing.T) {
	hbIsolateEnv(t)
	coord, _ := newCallOptionsTestCoordinator(t)
	sess, err := coord.sessions.Create(t.Context(), "hb-src-creds")
	require.NoError(t, err)
	cred := Credential{Provider: "tenant-provider", Type: ProviderTypeOpenAI, APIKey: "tenant-key"}

	both, err := coord.resolveCredentialsModels(t.Context(), sess.ID, &CredentialSet{
		Credentials: []Credential{cred},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant-provider", Model: "tenant-smart"},
			RoleFast:  {Provider: "tenant-provider", Model: "tenant-fast"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, hbSourcePerCall, both.smartSource)
	require.Equal(t, hbSourcePerCall, both.fastSource)

	fallback, err := coord.resolveCredentialsModels(t.Context(), sess.ID, &CredentialSet{
		Credentials:                 []Credential{cred},
		Models:                      map[Role]ModelChoice{RoleSmart: {Provider: "tenant-provider", Model: "tenant-smart"}},
		AllowConfiguredRoleFallback: true,
	})
	require.NoError(t, err)
	require.Equal(t, hbSourcePerCall, fallback.smartSource)
	require.Equal(t, hbSourceSlotStart, fallback.fastSource)
}
