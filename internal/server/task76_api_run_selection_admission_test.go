package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type task76SelectionAdmissionStore struct {
	APIWriteStore
	getCalls    int
	createCalls int
	onGet       func()
}

func (s *task76SelectionAdmissionStore) CreateSession(*http.Request, string, message.Origin) (session.Session, error) {
	s.createCalls++
	return session.Session{}, nil
}

func (s *task76SelectionAdmissionStore) GetSession(_ *http.Request, id string) (session.Session, error) {
	s.getCalls++
	if s.onGet != nil {
		s.onGet()
	}
	return session.Session{ID: id}, nil
}

func (*task76SelectionAdmissionStore) OwnsSession(session.Session) bool { return true }

type task76SelectionAdmissionBackend struct {
	busyCalls      int
	startCalls     int
	selectionCalls int
	role           string
	model          string
	selection      *apiRunSelection
}

func (b *task76SelectionAdmissionBackend) Start(_ context.Context, _, _, _, _, _ string) (<-chan RunOutcome, error) {
	b.startCalls++
	return task76SelectionOutcome(), nil
}

func (b *task76SelectionAdmissionBackend) StartWithSelection(_ context.Context, _, _, _, role, model string, _ int, selection *apiRunSelection) (<-chan RunOutcome, error) {
	b.startCalls++
	b.selectionCalls++
	b.role = role
	b.model = model
	b.selection = selection
	return task76SelectionOutcome(), nil
}

func (b *task76SelectionAdmissionBackend) Cancel(string, string) bool         { return false }
func (b *task76SelectionAdmissionBackend) Inject(string, string, string) bool { return false }
func (b *task76SelectionAdmissionBackend) Busy(string) bool {
	b.busyCalls++
	return false
}

func task76SelectionOutcome() <-chan RunOutcome {
	outcomes := make(chan RunOutcome, 1)
	outcomes <- RunOutcome{State: APIRunSucceeded}
	close(outcomes)
	return outcomes
}

func task76AdmissionConfig(models map[config.SelectedModelType]config.SelectedModel, roles map[string]config.CustomRole) *config.Config {
	cfg := &config.Config{Models: models, Roles: roles}
	cfg.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{
		"p": {ID: "p", Models: []catwalk.Model{{ID: "known"}}},
	})
	return cfg
}

func task76AdmissionHandler(t *testing.T, cfg *config.Config, store *task76SelectionAdmissionStore, backend *task76SelectionAdmissionBackend, registry *APIRunRegistry) http.Handler {
	t.Helper()
	handler := apiRunCreateHandlerWithSnapshot(store, backend, registry, func() (*config.Config, error) {
		return cfg, nil
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions/{id}/runs", handler)
	return mux
}

func TestTask76APIRunSelectionRejectsBeforeSessionAndAdmission(t *testing.T) {
	cases := []struct {
		name, role, model, detail string
		kind                      error
		cfg                       *config.Config
	}{
		{"smart empty", "smart", "", "invalid model: no smart model configured", ErrAPIRunBackendInvalidModel, task76AdmissionConfig(nil, nil)},
		{"worker empty", "worker", "", "invalid model: no worker model configured", ErrAPIRunBackendInvalidModel, task76AdmissionConfig(nil, nil)},
		{"smart partial", "smart", "", "invalid model: no smart model configured", ErrAPIRunBackendInvalidModel, task76AdmissionConfig(map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeSmart: {Provider: "p"},
		}, nil)},
		{"worker partial", "worker", "", "invalid model: no worker model configured", ErrAPIRunBackendInvalidModel, task76AdmissionConfig(map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeWorker: {Model: "known"},
		}, nil)},
		{"unknown custom", "missing", "", `invalid role: agent_role_unconfigured: custom role "missing"`, ErrAPIRunBackendInvalidRole, task76AdmissionConfig(nil, map[string]config.CustomRole{})},
		{"incomplete custom", "Broken", "", `invalid role: agent_role_unconfigured: custom role "Broken": custom role requires provider and model`, ErrAPIRunBackendInvalidRole, task76AdmissionConfig(nil, map[string]config.CustomRole{
			"Broken": {SelectedModel: config.SelectedModel{Provider: "p"}},
		})},
		{"invalid model", "", "p/unknown", "invalid model", ErrAPIRunBackendInvalidModel, task76AdmissionConfig(nil, nil)},
		{"custom with model", "Writer", "p/known", "invalid model", ErrAPIRunBackendInvalidModel, task76AdmissionConfig(nil, map[string]config.CustomRole{
			"Writer": {SelectedModel: config.SelectedModel{Provider: "p", Model: "known"}},
		})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiRunHandlerEnv(t)
			store := &task76SelectionAdmissionStore{}
			backend := &task76SelectionAdmissionBackend{}
			registry := NewAPIRunRegistry(nil)
			mux := task76AdmissionHandler(t, tc.cfg, store, backend, registry)
			_, validationErr := validateAPIRunSelection(tc.cfg, tc.role, tc.model)
			require.ErrorIs(t, validationErr, tc.kind)
			got := apiRunRequest(t, mux, http.MethodPost, "/api/v1/sessions/path-session/runs",
				`{"prompt":"prompt","role":"`+tc.role+`","model":"`+tc.model+`"}`,
				Principal{TokenID: "task76", Scopes: []string{"run"}}, nil)

			require.Equal(t, http.StatusBadRequest, got.status, "body=%s", got.body)
			var problem APIProblem
			require.NoError(t, json.Unmarshal(got.body, &problem))
			require.Equal(t, tc.detail, problem.Detail)
			require.Zero(t, store.getCalls, "invalid selection must precede session lookup")
			require.Zero(t, store.createCalls)
			require.Zero(t, backend.busyCalls)
			require.Zero(t, backend.startCalls)
			require.Zero(t, backend.selectionCalls)
			registry.mu.Lock()
			runCount := len(registry.runs)
			registry.mu.Unlock()
			require.Zero(t, runCount, "invalid selection must not reserve a registry record")
		})
	}
}

func TestTask76APIRunSelectionAdmitsOmittedConfiguredAndCustomRoles(t *testing.T) {
	t.Run("omitted without default", func(t *testing.T) {
		apiRunHandlerEnv(t)
		cfg := task76AdmissionConfig(nil, nil)
		store := &task76SelectionAdmissionStore{}
		backend := &task76SelectionAdmissionBackend{}
		registry := NewAPIRunRegistry(nil)
		got := apiRunRequest(t, task76AdmissionHandler(t, cfg, store, backend, registry), http.MethodPost,
			"/api/v1/sessions/path-session/runs", `{"prompt":"prompt"}`,
			Principal{TokenID: "task76", Scopes: []string{"run"}}, nil)
		require.Equal(t, http.StatusAccepted, got.status, "body=%s", got.body)
		require.Equal(t, 1, store.getCalls)
		require.Equal(t, 1, backend.busyCalls)
		require.Equal(t, 1, backend.startCalls)
		require.Equal(t, 1, backend.selectionCalls)
		require.NotNil(t, backend.selection)
		require.Same(t, cfg, backend.selection.Config)
		require.Nil(t, backend.selection.Contract)
	})

	t.Run("configured smart model", func(t *testing.T) {
		apiRunHandlerEnv(t)
		cfg := task76AdmissionConfig(map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeSmart: {Provider: "p", Model: "known", ReasoningEffort: "high"},
		}, nil)
		store := &task76SelectionAdmissionStore{}
		backend := &task76SelectionAdmissionBackend{}
		registry := NewAPIRunRegistry(nil)
		got := apiRunRequest(t, task76AdmissionHandler(t, cfg, store, backend, registry), http.MethodPost,
			"/api/v1/sessions/path-session/runs", `{"prompt":"prompt","role":"smart","model":"p/known"}`,
			Principal{TokenID: "task76", Scopes: []string{"run"}}, nil)
		require.Equal(t, http.StatusAccepted, got.status, "body=%s", got.body)
		require.Equal(t, 1, backend.selectionCalls)
		require.NotNil(t, backend.selection)
		require.Same(t, cfg, backend.selection.Config)
		require.Equal(t, "smart", backend.role)
		require.Equal(t, "p/known", backend.model)
		require.Equal(t, config.SelectedModel{Provider: "p", Model: "known", ReasoningEffort: "high"}, backend.selection.Config.Models[config.SelectedModelTypeSmart])
		require.Nil(t, backend.selection.Contract)
	})

	t.Run("custom contract is detached at admission", func(t *testing.T) {
		apiRunHandlerEnv(t)
		cfg := task76AdmissionConfig(nil, map[string]config.CustomRole{
			"Writer": {SelectedModel: config.SelectedModel{Provider: "p", Model: "known", ReasoningEffort: "high"}},
		})
		store := &task76SelectionAdmissionStore{
			onGet: func() {
				cfg.Roles["Writer"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "p", Model: "changed"}}
			},
		}
		backend := &task76SelectionAdmissionBackend{}
		registry := NewAPIRunRegistry(nil)
		got := apiRunRequest(t, task76AdmissionHandler(t, cfg, store, backend, registry), http.MethodPost,
			"/api/v1/sessions/path-session/runs", `{"prompt":"prompt","role":"Writer"}`,
			Principal{TokenID: "task76", Scopes: []string{"run"}}, nil)
		require.Equal(t, http.StatusAccepted, got.status, "body=%s", got.body)
		require.Equal(t, 1, backend.selectionCalls)
		require.NotNil(t, backend.selection)
		require.Same(t, cfg, backend.selection.Config)
		require.NotNil(t, backend.selection.Contract)
		require.Equal(t, "Writer", backend.selection.Contract.Name)
		require.Equal(t, "Writer", backend.role)
		require.Empty(t, backend.model)
		require.Equal(t, "known", backend.selection.Contract.Model.Model)
		require.Equal(t, "high", backend.selection.Contract.Model.ReasoningEffort)
	})
}
