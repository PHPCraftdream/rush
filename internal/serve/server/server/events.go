package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/version"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/serve/ops/update"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire/finalgate"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsapi"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

const batchInterval = 16 * time.Millisecond

// configNotificationsProcessed counts fully processed notifications; enqueued
// counts snapshots accepted by the hub's coalescing config queue. Test hooks.
var (
	configNotificationsProcessed atomic.Int64
	configNotificationsEnqueued  atomic.Int64
)

func forwardConfigSnapshot(h *Hub, wire ConfigWire, ok bool) {
	if ok && h.BroadcastConfig(wire) {
		configNotificationsEnqueued.Add(1)
	}
	configNotificationsProcessed.Add(1)
}

// Fork merge note: upstream rewrote events.go as REST /v1/... protocol
// conversions (sessionToProto, messageToProto, etc.). Our events.go is a
// WebSocket subscriber that forwards pubsub events to our Hub. Different
// architectures — keep ours. CHANGELOG.fork.md section 4.A.

type eventForwarders struct {
	join       func()
	syncConfig func()
}

func (f eventForwarders) Wait()       { f.join() }
func (f eventForwarders) SyncConfig() { f.syncConfig() }

// subscribeAndBroadcast subscribes to app event sources and forwards them to the hub.
func subscribeAndBroadcast(ctx context.Context, a *appPkg.App, h *Hub, gate session.FinalizationTerminalGate) eventForwarders {
	var forwarders sync.WaitGroup
	forwarders.Add(4)
	// Live-work snapshots: the forwarders below only MARK sessions dirty;
	// this worker does the DB reads off the broadcast hot path (#1058) and
	// exits with ctx.
	liveWork := startLiveWorkPusher(ctx, a, h)

	// Fork patch (finalization): nil gate (flag off) leaves both forwarders
	// unchanged; a held terminal event waits for the seal. WS and web
	// clients inherit the holds through the hub.

	// Sessions
	go func() {
		defer forwarders.Done()
		ch := finalgate.FinalizationGatedSessionStream(ctx, gate, a.Sessions.Subscribe(ctx))
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				switch ev.Type {
				case pubsub.CreatedEvent:
					sess := ev.Payload
					wsapi.AnnotateSessionExternalOwnership(a, &sess)
					h.Broadcast(EventSessionCreated, sess)
				case pubsub.UpdatedEvent:
					sess := ev.Payload
					wsapi.AnnotateSessionExternalOwnership(a, &sess)
					h.Broadcast(EventSessionUpdated, sess)
				case pubsub.DeletedEvent:
					h.Broadcast(EventSessionDeleted, ev.Payload)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Messages ΓÇö batch streaming updates to avoid flooding the client.
	go func() {
		defer forwarders.Done()
		ch := finalgate.FinalizationGatedMessageStream(ctx, gate, a.Messages.Subscribe(ctx))
		ticker := time.NewTicker(batchInterval)
		defer ticker.Stop()

		// pending holds the latest update per message ID within the current batch.
		pending := make(map[string]message.Message)

		flush := func() {
			for _, msg := range pending {
				h.Broadcast(EventMessageUpdated, wire.ToMessageWire(msg))
			}
			clear(pending)
		}

		for {
			select {
			case <-ctx.Done():
				flush()
				return
			case ev, ok := <-ch:
				if !ok {
					flush()
					return
				}
				switch ev.Type {
				case pubsub.CreatedEvent:
					h.Broadcast(EventMessageCreated, wire.ToMessageWire(ev.Payload))
					if shouldPushLiveWork(ev.Payload) {
						liveWork.mark(ev.Payload.SessionID)
					}
				case pubsub.UpdatedEvent:
					// Deduplicate: keep only the latest update per message ID.
					pending[ev.Payload.ID] = ev.Payload
				case pubsub.DeletedEvent:
					delete(pending, ev.Payload.ID)
					h.Broadcast(EventMessageDeleted, ev.Payload)
					if shouldPushLiveWork(ev.Payload) {
						liveWork.mark(ev.Payload.SessionID)
					}
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	// MCP state changes — broadcast a full snapshot of all servers on each
	// event. Read/broker surfaces are process-wide by design (shared
	// name-keyed registry + shared event bus); isolation via IsConfigured
	// filtering in buildMCPSnapshot below.
	go func() {
		defer forwarders.Done()
		ch := mcp.SubscribeEvents(ctx)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				h.Broadcast(EventMCPState, buildMCPSnapshot(a.Config()))
			case <-ctx.Done():
				return
			}
		}
	}()

	// Poll-driven config changes (plan §2.4): re-broadcast the full config
	// snapshot so web sessions that inherit their models display the new
	// defaults. The channel is coalescing (buffer 1), so bursts of reloads
	// cannot flood clients. The subscription is registered SYNCHRONOUSLY
	// before the forwarder goroutine starts: once subscribeAndBroadcast
	// returns, a poll-driven publish is guaranteed to land in ch (buffer 1)
	// even if it fires before the goroutine's first receive.
	type configBarrier struct{ done chan struct{} }
	barriers := make(chan configBarrier)
	configForwarderDone := make(chan struct{})
	ch := a.SubscribeModelsChanged(ctx)
	go func() {
		defer close(configForwarderDone)
		defer forwarders.Done()
		process := func() {
			wire, ok := buildConfigWire(a)
			forwardConfigSnapshot(h, wire, ok)
		}
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				process()
			case barrier := <-barriers:
				for {
					select {
					case _, ok := <-ch:
						if !ok {
							close(barrier.done)
							return
						}
						process()
					default:
						close(barrier.done)
						goto drained
					}
				}
			drained:
			}
		}
	}()

	slog.Debug("ws: event subscriptions started")

	// Unused imports guard — referenced through the generic channels above.
	_ = session.Session{}
	return eventForwarders{
		join: func() {
			forwarders.Wait()
			<-liveWork.stopped
		},
		syncConfig: func() {
			done := make(chan struct{})
			select {
			case barriers <- configBarrier{done: done}:
			case <-configForwarderDone:
				return
			}
			select {
			case <-done:
			case <-configForwarderDone:
			}
		},
	}
}

// buildMCPSnapshot returns the current state of all MCP servers in wire format.
func buildMCPSnapshot(cfg *config.Config) MCPSnapshot {
	all := mcp.GetStates()
	servers := make([]MCPServerInfo, 0, len(all))
	for name, info := range all {
		srv := MCPServerInfo{
			Name:      name,
			Status:    info.State.String(),
			Disabled:  info.State == mcp.StateDisabled,
			ToolCount: info.Counts.Tools,
			Tools:     mcp.GetServerToolNames(name),
		}
		if cfg != nil {
			if mcpCfg, ok := cfg.MCP[name]; ok {
				srv.ServerType = string(mcpCfg.Type)
				srv.Command = mcpCfg.Command
				srv.Args = mcpCfg.Args
				srv.URL = mcpCfg.URL
				srv.Env = mcpCfg.Env
				srv.Headers = mcpCfg.Headers
				srv.Source = string(mcpCfg.Source)
			}
		}
		servers = append(servers, srv)
	}
	return MCPSnapshot{Servers: servers}
}

// Startup update check for the web UI. This is the delivery path that
// replaced the one lost with the TUI: upstream published a pubsub message
// Bubble Tea rendered, the fork removed that UI, and for a while the check
// ran and discarded its answer (see the app.checkForUpdates removal).

// updateCheckTimeout bounds the startup check. It is best-effort
// decoration: a slow or unreachable release API must never delay the
// server becoming usable, and the check runs in its own goroutine anyway.
const updateCheckTimeout = 30 * time.Second

// broadcastUpdateNotice checks for a newer release once and, if one exists,
// tells every connected client. Thin production wrapper around
// broadcastUpdateNoticeWithClientVersion, always using the real GitHub
// client and the binary's own version.Version.
//
// Deliberately fire-and-forget with no retry and no schedule: the operator
// restarts rush often enough, and a background poller would be a network
// call nobody asked for on a machine that may be offline on purpose. A
// failed check is logged at debug — it is not a problem the user can act
// on, and at warn it would cry wolf on every offline start.
//
// Sends nothing when no update is available, so the client can render on
// receipt without deciding anything.
func broadcastUpdateNotice(ctx context.Context, h *Hub) {
	broadcastUpdateNoticeWithClientVersion(ctx, h, update.Default, version.Version)
}

// broadcastUpdateNoticeWithClientVersion is broadcastUpdateNotice's
// injectable core: current is the version string to compare against (the
// production caller always passes version.Version), and client is the
// update.Client used to fetch the latest release (the production caller
// always passes update.Default, the real GitHub API). Split out purely so
// a test can exercise this exact function — including the h.BroadcastSticky
// call at the bottom, the actual line that decides whether the update
// notice is sticky at all — with a fake Client instead of hitting the
// network or depending on this fork's actual current version string.
func broadcastUpdateNoticeWithClientVersion(ctx context.Context, h *Hub, client update.Client, current string) {
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()

	info, err := update.Check(checkCtx, current, client)
	if err != nil {
		slog.Debug("update check failed", "err", err)
		return
	}
	// A development build has nothing to compare against. Available() treats
	// any pre-release Current against a stable Latest as "an update exists",
	// so without this a locally built binary — "devel-<commit>", or the
	// current 0.2.0-alpha.19 — would raise the badge on EVERY start, forever.
	// IsDevelopment already knows every shape internal/version emits.
	if info.IsDevelopment() {
		slog.Debug("skipping update notice: development build", "version", info.Current)
		return
	}
	if !info.Available() {
		return
	}

	slog.Info("a newer rush is available", "current", info.Current, "latest", info.Latest)
	// Sticky, not a plain Broadcast: this is sent once at server start and
	// the replay ring evicts it within a single streaming turn, so an
	// operator who opens the browser after any real work would never see the
	// badge (task #547). Sticky delivery re-sends it on every client
	// register instead.
	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{
		Current: info.Current,
		Latest:  info.Latest,
	})
}
