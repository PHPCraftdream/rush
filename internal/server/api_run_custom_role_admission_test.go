package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type apiRunSelectionCapture struct {
	APIRunBackend
	selection *apiRunSelection
	starts    int
}

func (b *apiRunSelectionCapture) Start(ctx context.Context, _ string, _ string, _ string, _ string, _ string) (<-chan RunOutcome, error) {
	b.starts++
	return make(chan RunOutcome), nil
}

func (b *apiRunSelectionCapture) StartWithSelection(_ context.Context, _, _, _, _, _ string, _ int, selection *apiRunSelection) (<-chan RunOutcome, error) {
	b.starts++
	b.selection = selection
	outcomes := make(chan RunOutcome, 1)
	outcomes <- RunOutcome{State: APIRunSucceeded}
	return outcomes, nil
}

func (b *apiRunSelectionCapture) Busy(string) bool { return false }

type apiRunConfigMutationStore struct {
	APIWriteStore
	item   session.Session
	mutate func()
	reads  int
}

func (s *apiRunConfigMutationStore) GetSession(_ *http.Request, id string) (session.Session, error) {
	if s.mutate != nil {
		s.mutate()
	}
	s.reads++
	if id != s.item.ID {
		return session.Session{}, ErrAPIRunNotFound
	}
	return s.item, nil
}

func (s *apiRunConfigMutationStore) OwnsSession(session.Session) bool { return true }

func TestAPIRunCustomRolePinsAdmissionSnapshotBeforeBackend(t *testing.T) {
	apiRunHandlerEnv(t)
	patterns := []string{"view", "grep"}
	cfg := &config.Config{Roles: map[string]config.CustomRole{
		"Research Analyst": {
			SelectedModel: config.SelectedModel{Provider: "p", Model: "m1", ReasoningEffort: "high"},
			Tools:         &patterns,
		},
	}}
	cfg.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{"p": {ID: "p"}})
	backend := &apiRunSelectionCapture{}
	store := &apiRunConfigMutationStore{
		item: session.Session{ID: "s"},
		mutate: func() {
			cfg.Roles["Research Analyst"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "p", Model: "m2"}}
		},
	}
	registry := NewAPIRunRegistry(nil)
	handler := apiRunCreateHandlerWithSnapshot(store, backend, registry, func() (*config.Config, error) {
		return cfg, nil
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions/{id}/runs", handler)
	got := apiRunRequest(t, mux, http.MethodPost, "/api/v1/sessions/s/runs",
		`{"prompt":"p","role":"Research Analyst"}`, Principal{TokenID: "t", Scopes: []string{"run"}}, nil)
	require.Equal(t, http.StatusAccepted, got.status)
	require.Equal(t, 1, backend.starts)
	require.NotNil(t, backend.selection)
	require.NotNil(t, backend.selection.Contract)
	require.Equal(t, "Research Analyst", backend.selection.Contract.Name)
	require.Equal(t, "m1", backend.selection.Contract.Model.Model)
	require.Equal(t, "high", backend.selection.Contract.Model.ReasoningEffort)
	require.NotNil(t, backend.selection.Contract.Tools)
	require.Equal(t, []string{"view", "grep"}, *backend.selection.Contract.Tools)
}

func TestAPIRunUnknownCustomRoleRefusesBeforeRegistryAndBackend(t *testing.T) {
	apiRunHandlerEnv(t)
	cfg := &config.Config{Roles: map[string]config.CustomRole{}}
	cfg.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{"p": {ID: "p"}})
	backend := &apiRunSelectionCapture{}
	registry := NewAPIRunRegistry(nil)
	store := &apiRunConfigMutationStore{item: session.Session{ID: "s"}}
	handler := apiRunCreateHandlerWithSnapshot(store, backend, registry, func() (*config.Config, error) {
		return cfg, nil
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions/{id}/runs", handler)
	got := apiRunRequest(t, mux, http.MethodPost, "/api/v1/sessions/s/runs",
		`{"prompt":"p","role":"missing"}`, Principal{TokenID: "t", Scopes: []string{"run"}}, nil)
	require.Equal(t, http.StatusBadRequest, got.status)
	require.Zero(t, backend.starts)
	require.Zero(t, store.reads)
	registry.mu.Lock()
	count := len(registry.runs)
	registry.mu.Unlock()
	require.Zero(t, count)
}
