package server

import (
	"context"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type task76BackendCoordinator struct {
	agent.Coordinator
	mu       sync.Mutex
	reserves int
	starts   int
	last     *agent.ModelOverride
}

func (c *task76BackendCoordinator) ReserveExclusive(ctx context.Context, _ string) (context.Context, uint64, context.CancelFunc, bool) {
	c.mu.Lock()
	c.reserves++
	c.mu.Unlock()
	reserved, cancel := context.WithCancel(ctx)
	return reserved, 1, cancel, true
}

func (*task76BackendCoordinator) ReleaseExclusive(string, uint64, context.CancelFunc) {}
func (*task76BackendCoordinator) ResetAutoResumeCounter(string)                       {}
func (*task76BackendCoordinator) Cancel(string)                                       {}
func (*task76BackendCoordinator) CancelAll() bool { return false }

func (*task76BackendCoordinator) IsSessionBusy(string) bool { return false }

func (*task76BackendCoordinator) IsBusy() bool { return false }

func (c *task76BackendCoordinator) RunWithReservedOwnership(_ context.Context, _, _ string, _ uint64, _ context.CancelFunc, onHandoff func(), smart, _ *agent.ModelOverride, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	c.mu.Lock()
	c.starts++
	if smart == nil {
		c.last = nil
	} else {
		copy := *smart
		c.last = &copy
	}
	c.mu.Unlock()
	if onHandoff != nil {
		onHandoff()
	}
	return nil, nil
}

func (c *task76BackendCoordinator) snapshot() (reserves, starts int, smart *agent.ModelOverride) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last != nil {
		copy := *c.last
		smart = &copy
	}
	return c.reserves, c.starts, smart
}

func task76BackendApp(t *testing.T) (*appPkg.App, session.Session, *task76BackendCoordinator) {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	app.Store().SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{})
	sess, err := app.Sessions.Create(t.Context(), "task 76 backend")
	require.NoError(t, err)
	coord := &task76BackendCoordinator{}
	app.AgentCoordinator = coord
	return app, sess, coord
}

func task76SelectionConfig(models map[config.SelectedModelType]config.SelectedModel, roles map[string]config.CustomRole) *config.Config {
	cfg := &config.Config{Models: models, Roles: roles}
	cfg.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{
		"pinned-provider": {ID: "pinned-provider", Models: []catwalk.Model{{ID: "known-model"}, {ID: "custom-model"}}},
	})
	return cfg
}

func task76ReadonlyContext(t *testing.T) context.Context {
	t.Helper()
	return WithPrincipal(t.Context(), Principal{TokenID: "task76", Scopes: []string{"run"}, Permission: "readonly"})
}

func TestTask76BackendOmittedRoleLeavesDefaultToCoordinator(t *testing.T) {
	for _, useDepth := range []bool{false, true} {
		name := "selection"
		if useDepth {
			name = "depth"
		}
		t.Run(name, func(t *testing.T) {
			app, sess, coord := task76BackendApp(t)
			backend := newAPIRunBackend(app).(*appAPIRunBackend)
			var (
				done <-chan RunOutcome
				err  error
			)
			if useDepth {
				done, err = backend.StartWithDepth(task76ReadonlyContext(t), "task76-omitted-depth", sess.ID, "prompt", "", "", 2)
			} else {
				cfg := task76SelectionConfig(nil, nil)
				done, err = backend.StartWithSelection(task76ReadonlyContext(t), "task76-omitted-selection", sess.ID, "prompt", "", "", 2, &apiRunSelection{Config: cfg})
			}
			require.NoError(t, err)
			require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, done).State)
			reserves, starts, smart := coord.snapshot()
			require.Equal(t, 1, reserves)
			require.Equal(t, 1, starts)
			require.Nil(t, smart, "an omitted role with no default must leave selection to the coordinator")
		})
	}
}

func TestTask76BackendOmittedRoleUsesPinnedConfiguredDefault(t *testing.T) {
	app, sess, coord := task76BackendApp(t)
	app.Store().SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "app-provider", Model: "app-model", ReasoningEffort: "low"})
	pinned := task76SelectionConfig(map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "pinned-provider", Model: "known-model", ReasoningEffort: "high"},
	}, nil)
	backend := newAPIRunBackend(app).(*appAPIRunBackend)
	done, err := backend.StartWithSelection(task76ReadonlyContext(t), "task76-pinned-default", sess.ID, "prompt", "", "", 2, &apiRunSelection{Config: pinned})
	require.NoError(t, err)
	require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, done).State)
	reserves, starts, smart := coord.snapshot()
	require.Equal(t, 1, reserves)
	require.Equal(t, 1, starts)
	require.Equal(t, &agent.ModelOverride{Provider: "pinned-provider", Model: "known-model", ReasoningEffort: "high"}, smart)
}

func TestTask76BackendExplicitBuiltinAndModelOnlySelections(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		model    string
		config   *config.Config
		expected *agent.ModelOverride
	}{
		{
			name: "configured smart",
			role: "smart",
			config: task76SelectionConfig(map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeSmart: {Provider: "pinned-provider", Model: "known-model", ReasoningEffort: "high"},
			}, nil),
			expected: &agent.ModelOverride{Provider: "pinned-provider", Model: "known-model", ReasoningEffort: "high"},
		},
		{
			name:     "known model with omitted role",
			model:    "pinned-provider/known-model",
			config:   task76SelectionConfig(nil, nil),
			expected: &agent.ModelOverride{Provider: "pinned-provider", Model: "known-model"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, sess, coord := task76BackendApp(t)
			backend := newAPIRunBackend(app).(*appAPIRunBackend)
			done, err := backend.StartWithSelection(task76ReadonlyContext(t), "task76-positive-"+tc.name, sess.ID, "prompt", tc.role, tc.model, 2, &apiRunSelection{Config: tc.config})
			require.NoError(t, err)
			require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, done).State)
			reserves, starts, override := coord.snapshot()
			require.Equal(t, 1, reserves)
			require.Equal(t, 1, starts)
			require.Equal(t, tc.expected, override)
		})
	}
}

func TestTask76BackendExplicitSelectionRefusesBeforeSideEffects(t *testing.T) {
	writer := []string{"view", "grep"}
	custom := config.CustomRole{SelectedModel: config.SelectedModel{Provider: "pinned-provider", Model: "custom-model"}, Tools: &writer}
	cases := []struct {
		name      string
		role      string
		model     string
		selection *apiRunSelection
		depthPath bool
		errorKind error
	}{
		{name: "smart missing", role: "smart", selection: &apiRunSelection{Config: task76SelectionConfig(nil, nil)}, errorKind: ErrAPIRunBackendInvalidModel},
		{name: "smart partial", role: "smart", selection: &apiRunSelection{Config: task76SelectionConfig(map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeSmart: {Provider: "pinned-provider"}}, nil)}, errorKind: ErrAPIRunBackendInvalidModel},
		{name: "worker missing", role: "worker", depthPath: true, errorKind: ErrAPIRunBackendInvalidModel},
		{name: "worker missing despite model override", role: "worker", model: "pinned-provider/known-model", selection: &apiRunSelection{Config: task76SelectionConfig(nil, nil)}, errorKind: ErrAPIRunBackendInvalidModel},
		{name: "worker partial", role: "worker", selection: &apiRunSelection{Config: task76SelectionConfig(map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeWorker: {Model: "known-model"}}, nil)}, errorKind: ErrAPIRunBackendInvalidModel},
		{name: "unknown custom", role: "missing-profile", selection: &apiRunSelection{Config: task76SelectionConfig(nil, map[string]config.CustomRole{})}, errorKind: ErrAPIRunBackendInvalidRole},
		{name: "unknown pinned contract", role: "missing-profile", selection: func() *apiRunSelection {
			cfg := task76SelectionConfig(nil, map[string]config.CustomRole{})
			return &apiRunSelection{Config: cfg, Contract: &session.RoleContract{Version: session.RoleContractVersion, Name: "missing-profile", Model: session.ModelCfg{Model: "custom-model", Provider: "pinned-provider"}}}
		}(), errorKind: ErrAPIRunBackendInvalidRole},
		{name: "unconfigured custom through depth", role: "missing-profile", depthPath: true, errorKind: ErrAPIRunBackendInvalidRole},
		{name: "incomplete custom definition", role: "writer", selection: &apiRunSelection{Config: task76SelectionConfig(nil, map[string]config.CustomRole{"writer": {}})}, errorKind: ErrAPIRunBackendInvalidRole},
		{name: "malformed role", role: "bad/role", selection: &apiRunSelection{Config: task76SelectionConfig(nil, nil)}, errorKind: ErrAPIRunBackendInvalidRole},
		{name: "invalid explicit model", model: "pinned-provider/not-configured", selection: &apiRunSelection{Config: task76SelectionConfig(nil, nil)}, errorKind: ErrAPIRunBackendInvalidModel},
		{name: "custom plus model", role: "writer", model: "pinned-provider/known-model", selection: &apiRunSelection{Config: task76SelectionConfig(nil, map[string]config.CustomRole{"writer": custom})}, errorKind: ErrAPIRunBackendInvalidModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, sess, coord := task76BackendApp(t)
			backend := newAPIRunBackend(app).(*appAPIRunBackend)
			var err error
			if tc.depthPath {
				_, err = backend.StartWithDepth(task76ReadonlyContext(t), "task76-reject-"+tc.name, sess.ID, "prompt", tc.role, tc.model, 1)
			} else {
				_, err = backend.StartWithSelection(task76ReadonlyContext(t), "task76-reject-"+tc.name, sess.ID, "prompt", tc.role, tc.model, 1, tc.selection)
			}
			require.ErrorIs(t, err, tc.errorKind)
			reserves, starts, _ := coord.snapshot()
			require.Zero(t, reserves, "invalid selection must be rejected before reservation")
			require.Zero(t, starts, "invalid selection must be rejected before coordinator execution")
			require.False(t, app.Permissions.(interface{ SessionDenied(string) bool }).SessionDenied(sess.ID))
			require.False(t, app.Permissions.(interface{ SessionDenied(string) bool }).SessionDenied("task76-reject-"+tc.name), "invalid selection must not install a deny gate")
			active, _ := app.Permissions.(interface {
				SessionPolicyDecision(string, string, string) (bool, bool)
			}).SessionPolicyDecision(sess.ID, "view", "read")
			require.False(t, active, "invalid selection must not arm a permission policy")
			depth, depthErr := app.Sessions.(session.AgentDepthStore).ResolveSavedAgentDepth(t.Context(), sess.ID)
			require.NoError(t, depthErr)
			require.Equal(t, agent.DefaultAgentDepth, depth, "invalid selection must not persist a depth change")
		})
	}
}

func TestTask76BackendConfiguredCustomKeepsAdmittedContract(t *testing.T) {
	app, sess, coord := task76BackendApp(t)
	allowed := []string{"view", "grep"}
	cfg := task76SelectionConfig(nil, map[string]config.CustomRole{
		"writer": {SelectedModel: config.SelectedModel{Provider: "pinned-provider", Model: "custom-model", ReasoningEffort: "medium"}, Tools: &allowed},
	})
	contract, err := agent.ResolveRoleContract(cfg, "writer", nil)
	require.NoError(t, err)
	selection := &apiRunSelection{Config: cfg, Contract: contract.Clone()}
	cfg.Roles["writer"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "pinned-provider", Model: "known-model"}}

	_, _, admitted, err := apiRunBackendSelection(selection.Config, "writer", "", selection)
	require.NoError(t, err)
	require.True(t, contract.Equal(admitted), "backend must retain the pinned model and tool-ceiling snapshot")
	require.NotSame(t, selection.Contract, admitted)

	backend := newAPIRunBackend(app).(*appAPIRunBackend)
	done, err := backend.StartWithSelection(task76ReadonlyContext(t), "task76-custom", sess.ID, "prompt", "writer", "", 2, selection)
	require.NoError(t, err)
	require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, done).State)
	reserves, starts, smart := coord.snapshot()
	require.Equal(t, 1, reserves)
	require.Equal(t, 1, starts, "the admitted custom role should reach the coordinator")
	require.Nil(t, smart, "the custom role contract, not a built-in model override, selects its model")
}

var _ agent.Coordinator = (*task76BackendCoordinator)(nil)
