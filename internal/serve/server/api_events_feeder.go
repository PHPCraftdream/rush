package server

import (
	"context"
	"sync"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire/finalgate"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

type apiEventFeeder struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startAPIEventFeeder(app *appPkg.App, log *APIEventLog, registry *APIRunRegistry, gates ...session.FinalizationTerminalGate) *apiEventFeeder {
	ctx, cancel := context.WithCancel(context.Background())
	feeder := &apiEventFeeder{cancel: cancel, done: make(chan struct{})}
	if registry != nil {
		registry.setObserver(func(run APIRun) {
			data := struct {
				RunID     string      `json:"run_id"`
				State     APIRunState `json:"state"`
				ErrorCode string      `json:"error_code,omitempty"`
			}{RunID: run.ID, State: run.State}
			if run.Error != nil {
				data.ErrorCode = run.Error.Code
			}
			log.Publish(run.SessionID, "run", data)
		})
	}
	go func() {
		defer close(feeder.done)
		if app == nil {
			<-ctx.Done()
			return
		}
		// Fork patch (finalization): nil gate (flag off) leaves both feeds
		// byte-for-byte unchanged; a held terminal event waits for the seal.
		var gate session.FinalizationTerminalGate
		if len(gates) > 0 {
			gate = gates[0]
		} else {
			gate = finalgate.GateFor(app)
		}
		var wg sync.WaitGroup
		if app.Messages != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for event := range finalgate.FinalizationGatedMessageStream(ctx, gate, app.Messages.Subscribe(ctx)) {
					if event.Type == pubsub.CreatedEvent || event.Type == pubsub.UpdatedEvent {
						log.Publish(event.Payload.SessionID, "message", wire.APIMessageFromModel(event.Payload))
					}
				}
			}()
		}
		if app.Sessions != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for event := range finalgate.FinalizationGatedSessionStream(ctx, gate, app.Sessions.Subscribe(ctx)) {
					switch event.Type {
					case pubsub.CreatedEvent, pubsub.UpdatedEvent:
						log.Publish(event.Payload.ID, "session", wire.APISessionFromModel(event.Payload))
					case pubsub.DeletedEvent:
						log.DeleteSession(event.Payload.ID)
					}
				}
			}()
		}
		<-ctx.Done()
		wg.Wait()
	}()
	return feeder
}

func (f *apiEventFeeder) Stop(ctx context.Context) error {
	if f == nil {
		return nil
	}
	f.cancel()
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
