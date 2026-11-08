package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
)

var (
	ErrAPIRunBackendBusy            = errors.New("session is busy")
	ErrAPIRunBackendGateUnavailable = errors.New("permission gate unavailable")
	ErrAPIRunBackendCoordinator     = errors.New("agent coordinator unavailable")
	ErrAPIRunBackendNoActiveRun     = errors.New("no active run")
	ErrAPIRunBackendInvalidModel    = errors.New("invalid model")
	ErrAPIRunBackendInvalidRole     = errors.New("invalid role")
	ErrAPIRunAutoApproveDenied      = errors.New("auto-approve not permitted on this listener")
)

// RunOutcome is the backend's final result for an API-driven run. Message text is
// deliberately absent; registry records retain only the assistant message ID.
type RunOutcome struct {
	State           APIRunState
	ErrorCode       string
	ErrorSummary    string
	ResultMessageID string
}

// APIRunBackend adapts the application runner to API run lifecycle handlers.
type APIRunBackend interface {
	Start(ctx context.Context, runID, sessionID, prompt, role, model string) (done <-chan RunOutcome, err error)
	Cancel(sessionID, runID string) bool
	Inject(sessionID, answer, mode string) bool
	Busy(sessionID string) bool
}

// appAPIRunBackend runs API requests through the same coordinator path as the
// WebSocket send/inject handlers, without coupling the run to an HTTP request.
type appAPIRunBackend struct {
	app *appPkg.App

	broker           *APIPermissionBroker
	allowAutoApprove bool

	mu      sync.Mutex
	cancels map[string]apiRunBackendCancellation
}

type apiRunBackendCancellation struct {
	sessionID string
	cancel    context.CancelFunc
}

func newAPIRunBackend(app *appPkg.App) APIRunBackend {
	return newAPIRunBackendWithPolicy(app, nil, false)
}

func newAPIRunBackendWithPolicy(app *appPkg.App, broker *APIPermissionBroker, allowAutoApprove bool) APIRunBackend {
	return &appAPIRunBackend{app: app, broker: broker, allowAutoApprove: allowAutoApprove, cancels: make(map[string]apiRunBackendCancellation)}
}

func (b *appAPIRunBackend) Start(ctx context.Context, runID, sessionID, prompt, role, model string) (<-chan RunOutcome, error) {
	return b.StartWithDepth(ctx, runID, sessionID, prompt, role, model, agent.DefaultAgentDepth)
}

func (b *appAPIRunBackend) StartWithDepth(ctx context.Context, runID, sessionID, prompt, role, model string, maxAgentDepth int) (<-chan RunOutcome, error) {
	if maxAgentDepth < 0 || maxAgentDepth > 32 {
		return nil, fmt.Errorf("max_agent_depth must be between 0 and 32")
	}
	if b.app == nil || b.app.AgentCoordinator == nil {
		return nil, ErrAPIRunBackendCoordinator
	}
	if runID == "" || sessionID == "" || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("invalid run parameters")
	}
	maxAgentDepth, err := agent.PersistSessionAgentDepth(ctx, b.app.Sessions, sessionID, maxAgentDepth)
	if err != nil {
		return nil, fmt.Errorf("resolve API session agent depth: %w", err)
	}
	var (
		manager  permission.SessionDenyManager
		policies permission.SessionPolicyManager
	)
	if b.app.Permissions != nil {
		manager, _ = b.app.Permissions.(permission.SessionDenyManager)
		policies, _ = b.app.Permissions.(permission.SessionPolicyManager)
	}
	if manager == nil {
		return nil, ErrAPIRunBackendGateUnavailable
	}
	if ctx == nil || ctx.Err() != nil {
		return nil, context.Canceled
	}
	principal, hasPrincipal := PrincipalFrom(ctx)
	mode := principal.Permission
	if !hasPrincipal {
		mode = "invalid"
	} else if mode == "" {
		mode = "ask"
	}
	if mode != "ask" && mode != "readonly" && mode != "auto" {
		mode = "invalid"
	}
	if (mode == "readonly" || mode == "auto") && policies == nil {
		return nil, ErrAPIRunBackendGateUnavailable
	}
	if mode == "auto" && !config.APITokenScopeAllows(principal.Scopes, "admin") && !b.allowAutoApprove {
		return nil, ErrAPIRunAutoApproveDenied
	}
	if mode == "auto" {
		slog.Warn("API run uses auto-approve", "token_id", principal.TokenID)
	}
	cfg := b.app.Config()
	if cfg == nil {
		return nil, ErrAPIRunBackendInvalidModel
	}
	roleType, err := apiRunRole(role)
	if err != nil {
		return nil, err
	}
	var smart *agent.ModelOverride
	if model != "" {
		smart, err = apiRunModelOverride(cfg, model)
	} else if selected, exists := cfg.Models[roleType]; exists {
		smart = &agent.ModelOverride{Provider: selected.Provider, Model: selected.Model, ReasoningEffort: selected.ReasoningEffort}
	}
	if err != nil {
		return nil, err
	}

	// Reserve first: only the winner may check and arm the per-run deny gate.
	_, epoch, reserveCancel, reserved := b.app.AgentCoordinator.ReserveExclusive(context.WithoutCancel(ctx), sessionID)
	if !reserved {
		return nil, ErrAPIRunBackendBusy
	}
	reservationOwned := true
	defer func() {
		if reservationOwned {
			b.app.AgentCoordinator.ReleaseExclusive(sessionID, epoch, reserveCancel)
		}
	}()
	if ctx.Err() != nil {
		return nil, context.Canceled
	}

	probe, refuse, _ := holdExternalSilenceProof(b.app, sessionID)
	if refuse {
		return nil, ErrAPIRunBackendBusy
	}
	if manager.SessionDenied(sessionID) {
		probe.Release()
		return nil, ErrAPIRunBackendBusy
	}
	brokerRegistered := false
	switch mode {
	case "ask":
		if b.broker == nil {
			probe.Release()
			return nil, ErrAPIRunBackendGateUnavailable
		}
		if err := b.broker.Register(runID, sessionID, principal.TokenID); err != nil {
			probe.Release()
			return nil, ErrAPIRunBackendGateUnavailable
		}
		brokerRegistered = true
	case "readonly":
		policies.SetSessionReadonly(sessionID, runID)
	case "auto":
		policies.SetSessionAuto(sessionID, runID)
	default:
		manager.DenySession(sessionID, runID)
	}
	probe.Release()

	gateArmed := mode != "ask"
	defer func() {
		if gateArmed {
			if mode == "invalid" {
				manager.ReleaseSessionDeny(sessionID, runID)
			} else if mode != "ask" {
				policies.ReleaseSessionPolicy(sessionID, runID)
			}
		}
		if brokerRegistered {
			b.broker.Release(runID)
		}
	}()
	if ctx.Err() != nil {
		return nil, context.Canceled
	}

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	stopCancel := context.AfterFunc(ctx, cancelRun)
	b.mu.Lock()
	if b.cancels == nil {
		b.cancels = make(map[string]apiRunBackendCancellation)
	}
	b.cancels[runID] = apiRunBackendCancellation{sessionID: sessionID, cancel: cancelRun}
	b.mu.Unlock()
	recorder := agent.NewCallResultRecorder()
	runCtx = agent.WithCallResultRecorder(runCtx, recorder)
	runCtx = agent.WithCallOptions(agent.WithCallOrigin(runCtx, message.OriginAPI), &agent.CallOptions{
		WebTurnOptIn: true, ModelRole: roleType, MaxAgentDepth: maxAgentDepth, AgentDepthSet: true,
	})
	b.app.AgentCoordinator.ResetAutoResumeCounter(sessionID)
	done := make(chan RunOutcome, 1)
	reservationOwned = false // the completion goroutine now owns reservation cleanup.
	gateArmed = false        // completion goroutine owns the gate release from here.
	completionBrokerRegistered := brokerRegistered
	brokerRegistered = false // completion goroutine owns the broker registration from here.
	go func() {
		handedOff := false
		var cleanupOnce sync.Once
		cleanup := func() {
			cleanupOnce.Do(func() {
				b.mu.Lock()
				delete(b.cancels, runID)
				b.mu.Unlock()
				stopCancel()
				cancelRun()
				if !handedOff {
					b.app.AgentCoordinator.ReleaseExclusive(sessionID, epoch, reserveCancel)
				}
				if mode == "invalid" {
					manager.ReleaseSessionDeny(sessionID, runID)
				} else if mode != "ask" {
					policies.ReleaseSessionPolicy(sessionID, runID)
				}
				if completionBrokerRegistered {
					b.broker.Release(runID)
				}
			})
		}
		defer cleanup()
		_, runErr := b.app.AgentCoordinator.RunWithReservedOwnership(runCtx, sessionID, prompt, epoch, reserveCancel, func() { handedOff = true }, smart, nil)
		outcome := RunOutcome{State: APIRunSucceeded}
		if runErr != nil {
			outcome.State = APIRunFailed
			outcome.ErrorCode = "run_failed"
			outcome.ErrorSummary = "run failed"
			var awaiting *agent.AwaitingAnswerError
			if errors.As(runErr, &awaiting) {
				outcome.State = APIRunAwaitingAnswer
				outcome.ErrorCode = "awaiting_answer"
				outcome.ErrorSummary = "run is awaiting an answer; resume via the remote REST API using the session endpoint"
			}
		}
		if errors.Is(runErr, context.Canceled) || runCtx.Err() != nil {
			outcome.State = APIRunCancelled
			outcome.ErrorCode = "cancelled"
			outcome.ErrorSummary = "run cancelled"
		}
		recorder.Seal()
		outcome.ResultMessageID = recorder.Resolve()
		// Drop the cancel entry BEFORE lifting the deny gate: Cancel is
		// session-scoped in the coordinator, so a stale entry that outlives
		// the gate could stop a later run on the same session.
		b.mu.Lock()
		delete(b.cancels, runID)
		b.mu.Unlock()
		// Do not publish completion until run cleanup has released the session
		// reservation and permission gate; callers may immediately start a rerun.
		cleanup()
		done <- outcome
		close(done)
	}()
	return done, nil
}

func (b *appAPIRunBackend) Cancel(sessionID, runID string) bool {
	if b.app == nil || b.app.AgentCoordinator == nil {
		return false
	}
	b.mu.Lock()
	cancellation, ok := b.cancels[runID]
	if !ok || cancellation.sessionID != sessionID {
		b.mu.Unlock()
		return false
	}
	// Keep this run registered until the coordinator stop is issued. The
	// coordinator stop is session-scoped, so issue it before cancelling the
	// run context can release exclusive ownership for a later run.
	b.app.AgentCoordinator.Cancel(sessionID)
	cancellation.cancel()
	b.mu.Unlock()
	return true
}

func (b *appAPIRunBackend) Inject(sessionID, answer, mode string) bool {
	if b.app == nil || b.app.AgentCoordinator == nil || !b.app.AgentCoordinator.IsSessionBusy(sessionID) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = agent.WithCallOptions(agent.WithCallOrigin(ctx, message.OriginAPI), &agent.CallOptions{WebTurnOptIn: true})
	var err error
	switch mode {
	case "", "merge":
		_, err = b.app.AgentCoordinator.InjectMessage(ctx, sessionID, answer)
	case "interrupt":
		err = b.app.AgentCoordinator.InterruptAndSend(ctx, sessionID, answer, nil, nil)
	default:
		return false
	}
	return err == nil
}

func (b *appAPIRunBackend) Busy(sessionID string) bool {
	if b.app == nil {
		return true
	}
	if b.app.AgentCoordinator != nil && b.app.AgentCoordinator.IsSessionBusy(sessionID) {
		return true
	}
	if b.app.Permissions != nil {
		if manager, ok := b.app.Permissions.(permission.SessionDenyManager); ok && manager.SessionDenied(sessionID) {
			return true
		}
	}
	probe, refuse, _ := holdExternalSilenceProof(b.app, sessionID)
	if refuse {
		return true
	}
	probe.Release()
	return false
}

func apiRunRole(role string) (config.SelectedModelType, error) {
	if role == "" {
		return config.SelectedModelTypeSmart, nil
	}
	parsed := config.SelectedModelType(role)
	switch parsed {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return parsed, nil
	default:
		return "", ErrAPIRunBackendInvalidRole
	}
}

func apiRunModelOverride(cfg *config.Config, name string) (*agent.ModelOverride, error) {
	provider, model, hasProvider := strings.Cut(name, "/")
	if hasProvider {
		pc, ok := cfg.Providers.Get(provider)
		if !ok || pc.Disable || cfg.GetModel(provider, model) == nil {
			return nil, ErrAPIRunBackendInvalidModel
		}
		return &agent.ModelOverride{Provider: provider, Model: model}, nil
	}
	var match *agent.ModelOverride
	for _, pc := range cfg.EnabledProviders() {
		for _, configured := range pc.Models {
			if configured.ID != name {
				continue
			}
			if match != nil {
				return nil, fmt.Errorf("%w: model ID is ambiguous", ErrAPIRunBackendInvalidModel)
			}
			match = &agent.ModelOverride{Provider: pc.ID, Model: name}
		}
	}
	if match == nil {
		return nil, ErrAPIRunBackendInvalidModel
	}
	return match, nil
}

var _ APIRunBackend = (*appAPIRunBackend)(nil)
