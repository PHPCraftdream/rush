package app

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/shell"
)

type noticeConfiglessDiag struct {
	phase       string
	configured  bool
	coordinator bool
	errClass    string
}

func registerNoticeConfiglessDiagnostics(t *testing.T, d *noticeConfiglessDiag) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("notice-configless phase=%s configured=%t coordinator=%t err_class=%s", d.phase, d.configured, d.coordinator, d.errClass)
		}
	})
}

func noticeConfiglessErrorClass(err error) string {
	if err == nil {
		return "none"
	}
	if err.Error() == "smart model not selected" {
		return "smart-model-not-selected"
	}
	return "other"
}

func newNoticeConfiglessApp(t *testing.T, selectModels bool) *App {
	t.Helper()

	models := make(map[config.SelectedModelType]config.SelectedModel)
	cfg := &config.Config{
		Options: &config.Options{
			DataDirectory:   "",
			NoRealWorkspace: true,
			Attribution:     &config.Attribution{TrailerStyle: config.TrailerStyleNone},
		},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Models:    models,
		Agents: map[string]config.Agent{
			config.AgentCoder: {
				ID:    config.AgentCoder,
				Name:  "Coder",
				Model: config.SelectedModelTypeSmart,
			},
		},
	}
	cfg.Providers.Set("offline", config.ProviderConfig{
		ID:      "offline",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "unused-test-key",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	if selectModels {
		selected := config.SelectedModel{Provider: "offline", Model: "probe"}
		models[config.SelectedModelTypeSmart] = selected
		models[config.SelectedModelTypeFast] = selected
	}

	store := config.NewLibraryStore(cfg, t.TempDir())
	return &App{config: store}
}

func cleanupNoticeConfiglessApp(app *App) {
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.CancelAll()
	}
	if app.BackgroundShellManager != nil {
		app.BackgroundShellManager.Close(context.Background())
	}
}

func TestInitCoderAgent_ConfiglessNoticeStoreIsOptional(t *testing.T) {
	diag := noticeConfiglessDiag{phase: "environment", errClass: "none"}
	registerNoticeConfiglessDiagnostics(t, &diag)
	isolateAppNewTestEnv(t)

	app := newNoticeConfiglessApp(t, true)
	t.Cleanup(func() { cleanupNoticeConfiglessApp(app) })
	diag.configured = app.config.Config().IsConfigured()
	diag.coordinator = app.AgentCoordinator != nil

	diag.phase = "nil-writer"
	if app.batchCutoverWriter() != nil {
		t.Fatal("configless fixture unexpectedly has a batch cutover writer")
	}
	diag.phase = "nil-notice-store"
	if app.batchNoticeStore() != nil {
		t.Fatal("configless fixture unexpectedly has a batch notice store")
	}

	diag.phase = "init-coder-agent"
	err := app.InitCoderAgent(context.Background())
	diag.errClass = noticeConfiglessErrorClass(err)
	diag.coordinator = app.AgentCoordinator != nil
	if err != nil {
		t.Fatalf("InitCoderAgent failed (error class %s)", diag.errClass)
	}
	if app.AgentCoordinator == nil {
		t.Fatal("InitCoderAgent succeeded without assigning a coordinator")
	}
}

func TestInitCoderAgent_PropagatesCoordinatorConstructionError(t *testing.T) {
	diag := noticeConfiglessDiag{phase: "environment", errClass: "none"}
	registerNoticeConfiglessDiagnostics(t, &diag)
	isolateAppNewTestEnv(t)

	app := newNoticeConfiglessApp(t, false)
	app.BackgroundShellManager = shell.NewBackgroundShellManager()
	t.Cleanup(func() { cleanupNoticeConfiglessApp(app) })
	diag.configured = app.config.Config().IsConfigured()
	diag.coordinator = app.AgentCoordinator != nil

	diag.phase = "init-coder-agent"
	err := app.InitCoderAgent(context.Background())
	diag.errClass = noticeConfiglessErrorClass(err)
	diag.coordinator = app.AgentCoordinator != nil
	if diag.errClass != "smart-model-not-selected" {
		t.Fatalf("unexpected coordinator construction error class: %s", diag.errClass)
	}
	if app.AgentCoordinator != nil {
		t.Fatal("failed coordinator construction unexpectedly assigned a coordinator")
	}
}
