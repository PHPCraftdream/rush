// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Rush application.
package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/home"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func parseLevel(level mcp.LoggingLevel) slog.Level {
	switch level {
	case "info":
		return slog.LevelInfo
	case "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// ClientSession wraps an mcp.ClientSession with a context cancel function so
// that the context created during session establishment is properly cleaned up
// on close.
type ClientSession struct {
	*mcp.ClientSession
	cancel context.CancelFunc
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	s.cancel()
	return s.ClientSession.Close()
}

var (
	sessions = csync.NewMap[string, *ClientSession]()
	states   = csync.NewMap[string, ClientInfo]()
	broker   = pubsub.NewBroker[Event]()

	lifecycleMu sync.Mutex
	owner       *Owner
	initDone    = closedChannel()
	generation  uint64
)

// ErrOwnerBusy reports that another application currently owns the process
// wide MCP registry. The SDK deliberately permits only one application-mode
// owner; library-mode Apps do not acquire this owner.
var ErrOwnerBusy = errors.New("mcp: application owner is already active")

// Owner is the lifetime token for the process-wide MCP registry. The MCP
// package predates multiple App instances and its tool/state maps remain
// process-wide, so ownership is explicit rather than silently shared.
type Owner struct {
	implicit        bool
	closing         bool
	generation      uint64
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	initCount       int
	initWG          sync.WaitGroup
	initDoneOnce    sync.Once
	closeOnce       sync.Once
	closeDone       chan struct{}
	closeErr        error
}

func closedChannel() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Acquire reserves the process-wide MCP registry for an application.
func Acquire() (*Owner, error) {
	return acquire(false)
}

// acquireImplicit supports the legacy package-level entry points. Unlike an
// App-owned token, an idle implicit owner may be reclaimed after its registry
// has been emptied by its caller.
func acquireImplicit() (*Owner, error) {
	return acquire(true)
}

func acquire(implicit bool) (*Owner, error) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()

	if owner != nil {
		if !owner.implicit || owner.closing || owner.initCount != 0 || sessions.Len() != 0 ||
			states.Len() != 0 || allTools.Len() != 0 || allPrompts.Len() != 0 ||
			allResources.Len() != 0 {
			return nil, ErrOwnerBusy
		}
		owner.lifecycleCancel()
		resetRegistryLocked()
	}

	generation++
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	o := &Owner{
		implicit:        implicit,
		generation:      generation,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		closeDone:       make(chan struct{}),
	}
	owner = o
	initDone = make(chan struct{})
	return o, nil
}

func currentOwner() *Owner {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return owner
}

func (o *Owner) isCurrentLocked() bool {
	return owner == o && !o.closing
}

func (o *Owner) beginInit() bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return false
	}
	o.initCount++
	o.initWG.Add(1)
	return true
}

func (o *Owner) endInit() {
	lifecycleMu.Lock()
	o.initCount--
	lifecycleMu.Unlock()
	o.initWG.Done()
}

func (o *Owner) finishInitialize() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if owner == o {
		o.initDoneOnce.Do(func() { close(initDone) })
	}
}

func (o *Owner) acceptsSession() bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return o.isCurrentLocked()
}

func (o *Owner) commitRenewal(generation uint64, name string, session *ClientSession, counts Counts) bool {
	lifecycleMu.Lock()
	if owner != o || o.closing || o.generation != generation {
		lifecycleMu.Unlock()
		_ = session.Close()
		return false
	}
	sessions.Set(name, session)
	updateState(name, StateConnected, nil, session, counts)
	lifecycleMu.Unlock()
	return true
}

func (o *Owner) operationContext(ctx context.Context) (context.Context, func()) {
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.lifecycleCtx, cancel)
	return operationCtx, func() {
		stop()
		cancel()
	}
}

// Close stops all initialization before taking its session snapshot. This
// barrier is what prevents a session created after an old snapshot from
// escaping cleanup. Startup is cancelled first, but the initialization
// goroutines are still joined so their transports cannot outlive Close.
func (o *Owner) Close(ctx context.Context) error {
	o.closeOnce.Do(func() {
		lifecycleMu.Lock()
		if owner != o {
			lifecycleMu.Unlock()
			close(o.closeDone)
			return
		}
		o.closing = true
		o.lifecycleCancel()
		lifecycleMu.Unlock()

		go o.finishClose()
	})

	select {
	case <-o.closeDone:
		return o.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Owner) finishClose() {
	// Do not abandon this wait when a caller's cleanup context expires. The
	// owner remains the lifecycle fence until every admitted operation exits.
	o.initWG.Wait()

	var wg sync.WaitGroup
	for name, session := range sessions.Seq2() {
		wg.Go(func() {
			if err := session.Close(); err != nil &&
				!errors.Is(err, io.EOF) &&
				!errors.Is(err, context.Canceled) &&
				err.Error() != "signal: killed" {
				slog.Warn("Failed to shutdown MCP client", "name", name, "error", err)
			}
		})
	}
	wg.Wait()

	lifecycleMu.Lock()
	if owner == o {
		resetRegistryLocked()
		owner = nil
		initDone = closedChannel()
	}
	close(o.closeDone)
	lifecycleMu.Unlock()
}

func resetRegistryLocked() {
	for name := range sessions.Seq2() {
		sessions.Del(name)
	}
	for name := range states.Seq2() {
		states.Del(name)
	}
	for name := range allTools.Seq2() {
		allTools.Del(name)
	}
	for name := range allPrompts.Seq2() {
		allPrompts.Del(name)
	}
	for name := range allResources.Seq2() {
		allResources.Del(name)
	}
	broker.Shutdown()
	broker = pubsub.NewBroker[Event]()
}

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateError
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time
}

// SubscribeEvents returns a channel for MCP events
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return currentBroker().Subscribe(ctx)
}

func currentBroker() *pubsub.Broker[Event] {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return broker
}

func publishEvent(t pubsub.EventType, event Event) {
	currentBroker().Publish(t, event)
}

// GetStates returns the current state of all MCP clients
func GetStates() map[string]ClientInfo {
	return states.Copy()
}

// GetState returns the state of a specific MCP client
func GetState(name string) (ClientInfo, bool) {
	return states.Get(name)
}

// Close closes all MCP clients. This should be called during application shutdown.
func Close(ctx context.Context) error {
	o := currentOwner()
	if o == nil {
		return nil
	}
	return o.Close(ctx)
}

// Initialize initializes MCP clients based on the provided configuration.
//
// restrictToCLIEnabled, when true, additionally skips every server whose
// config does not set EnabledInCLI — set by internal/app.New's
// RestrictMCPToCLI option for non-interactive invocations (rush run and
// every other CLI subcommand except the bare `rush` that starts the web
// UI). The interactive web/TUI path always passes false here, so it
// keeps starting every non-disabled server exactly as before this field
// existed.
func Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore, restrictToCLIEnabled bool) {
	o := currentOwner()
	if o == nil {
		var err error
		o, err = acquireImplicit()
		if err != nil {
			slog.Error("Failed to acquire MCP application owner", "error", err)
			return
		}
	}
	o.Initialize(ctx, permissions, cfg, restrictToCLIEnabled)
}

// Initialize initializes MCP clients using this owner's lifecycle barrier.
func (o *Owner) Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore, restrictToCLIEnabled bool) {
	slog.Info("Initializing MCP clients")
	// The permission service is consumed later while tools are called. Keep it
	// in the signature for compatibility with the existing startup contract.
	_ = permissions
	var wg sync.WaitGroup
	initCtx, cancel := context.WithCancel(ctx)
	lifecycleMu.Lock()
	if owner != o || o.closing {
		lifecycleMu.Unlock()
		cancel()
		return
	}
	lifecycleMu.Unlock()
	if !o.beginInit() {
		cancel()
		return
	}
	defer o.endInit()
	stopOwner := context.AfterFunc(o.lifecycleCtx, cancel)
	defer stopOwner()
	defer cancel()
	// Initialize states for all configured MCPs
	for name, m := range cfg.Config().MCP {
		if !o.acceptsSession() {
			break
		}
		if m.Disabled {
			updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping disabled MCP", "name", name)
			continue
		}
		if restrictToCLIEnabled && !m.EnabledInCLI {
			updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping MCP not enabled for CLI mode (set enabled_in_cli or pass --all-mcp)", "name", name)
			continue
		}

		// Set initial starting state.
		wg.Add(1)
		go func(name string, m config.MCPConfig) {
			defer func() {
				wg.Done()
				if r := recover(); r != nil {
					var err error
					switch v := r.(type) {
					case error:
						err = v
					case string:
						err = fmt.Errorf("panic: %s", v)
					default:
						err = fmt.Errorf("panic: %v", v)
					}
					updateState(name, StateError, err, nil, Counts{})
					slog.Error("Panic in MCP client initialization", "error", err, "name", name)
				}
			}()

			if err := initClient(initCtx, cfg, name, m, cfg.Resolver()); err != nil {
				slog.Debug("Failed to initialize MCP client", "name", name, "error", err)
			}
		}(name, m)
	}
	wg.Wait()
	o.finishInitialize()
}

// WaitForInit blocks until MCP initialization is complete.
// If Initialize was never called, this returns immediately.
func WaitForInit(ctx context.Context) error {
	lifecycleMu.Lock()
	done := initDone
	lifecycleMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InitializeSingle initializes a single MCP client by name.
func InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	if currentOwner() == nil {
		var err error
		_, err = acquireImplicit()
		if err != nil {
			return err
		}
	}
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if m.Disabled {
		updateState(name, StateDisabled, nil, nil, Counts{})
		slog.Debug("Skipping disabled MCP", "name", name)
		return nil
	}

	return initClient(ctx, cfg, name, m, cfg.Resolver())
}

// initClient initializes a single MCP client with the given configuration.
func initClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver) error {
	if o := currentOwner(); o != nil {
		if !o.beginInit() {
			return ErrOwnerBusy
		}
		defer o.endInit()
	}
	// Set initial starting state.
	updateState(name, StateStarting, nil, nil, Counts{})

	// createSession handles its own timeout internally.
	session, err := createSession(ctx, name, m, resolver)
	if err != nil {
		return err
	}

	tools, err := getTools(ctx, session)
	if err != nil {
		slog.Error("Error listing tools", "error", err, "name", name)
		updateState(name, StateError, err, nil, Counts{})
		session.Close()
		return err
	}

	prompts, err := getPrompts(ctx, session)
	if err != nil {
		slog.Error("Error listing prompts", "error", err, "name", name)
		updateState(name, StateError, err, nil, Counts{})
		session.Close()
		return err
	}

	if o := currentOwner(); o != nil && !o.acceptsSession() {
		_ = session.Close()
		return ErrOwnerBusy
	}
	toolCount := updateTools(cfg, name, tools)
	updatePrompts(name, prompts)
	sessions.Set(name, session)

	updateState(name, StateConnected, nil, session, Counts{
		Tools:   toolCount,
		Prompts: len(prompts),
	})

	return nil
}

// DisableSingle disables and closes a single MCP client by name.
func DisableSingle(cfg *config.ConfigStore, name string) error {
	session, ok := sessions.Get(name)
	if ok {
		if err := session.Close(); err != nil &&
			!errors.Is(err, io.EOF) &&
			!errors.Is(err, context.Canceled) &&
			err.Error() != "signal: killed" {
			slog.Warn("Error closing MCP session", "name", name, "error", err)
		}
		sessions.Del(name)
	}

	// Clear tools and prompts for this MCP.
	updateTools(cfg, name, nil)
	updatePrompts(name, nil)

	// Update state to disabled.
	updateState(name, StateDisabled, nil, nil, Counts{})

	slog.Info("Disabled mcp client", "name", name)
	return nil
}

// DisableServer disables an MCP server: closes its session, removes its tools,
// and persists the disabled flag to config.
func DisableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	mcpCfg, ok := cfg.Config().MCP[name]
	if !ok {
		return fmt.Errorf("MCP server %q not found", name)
	}

	_ = DisableSingle(cfg, name)

	// Update in-memory config
	mcpCfg.Disabled = true
	c := cfg.Config()
	c.MCP[name] = mcpCfg

	// External servers (.mcp.json) persist disabled state to workspace config;
	// user-configured servers persist to global config.
	scope := config.ScopeGlobal
	if mcpCfg.Source == config.MCPSourceExternal {
		scope = config.ScopeWorkspace
	}
	if err := cfg.SetConfigField(scope, fmt.Sprintf("mcp.%s.disabled", name), true); err != nil {
		slog.Warn("Failed to persist MCP disabled state", "name", name, "err", err)
	}

	return nil
}

// EnableServer re-enables a disabled MCP server and starts a new session.
func EnableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	mcpCfg, ok := cfg.Config().MCP[name]
	if !ok {
		return fmt.Errorf("MCP server %q not found", name)
	}

	// Update in-memory config
	mcpCfg.Disabled = false
	c := cfg.Config()
	c.MCP[name] = mcpCfg

	// External servers (.mcp.json) persist enabled state to workspace config;
	// user-configured servers persist to global config.
	scope := config.ScopeGlobal
	if mcpCfg.Source == config.MCPSourceExternal {
		scope = config.ScopeWorkspace
	}
	if err := cfg.SetConfigField(scope, fmt.Sprintf("mcp.%s.disabled", name), false); err != nil {
		slog.Warn("Failed to persist MCP enabled state", "name", name, "err", err)
	}

	updateState(name, StateStarting, nil, nil, Counts{})
	if currentOwner() == nil {
		var err error
		_, err = acquireImplicit()
		if err != nil {
			return err
		}
	}
	go func() {
		if err := initClient(ctx, cfg, name, mcpCfg, cfg.Resolver()); err != nil {
			slog.Error("Failed to enable MCP server", "name", name, "err", err)
		}
	}()
	return nil
}

// AddServer validates and adds a new MCP server. It attempts to connect; if
// successful the server is added to the in-memory config and persisted to disk.
func AddServer(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) error {
	if currentOwner() == nil {
		var err error
		_, err = acquireImplicit()
		if err != nil {
			return err
		}
	}
	c := cfg.Config()
	if _, exists := c.MCP[name]; exists {
		return fmt.Errorf("MCP server %q already exists", name)
	}

	// Ensure MCP map is initialised
	if c.MCP == nil {
		c.MCP = make(config.MCPs)
	}

	// Optimistically add to in-memory so initClient can read DisabledTools etc.
	c.MCP[name] = mcpCfg
	updateState(name, StateStarting, nil, nil, Counts{})

	initErr := initClient(ctx, cfg, name, mcpCfg, cfg.Resolver())
	if errors.Is(initErr, ErrOwnerBusy) {
		delete(c.MCP, name)
		return ErrOwnerBusy
	}
	if initErr != nil {
		delete(c.MCP, name)
		states.Del(name)
		return fmt.Errorf("failed to connect to MCP server %q: %w", name, initErr)
	}

	// Persist to config file
	if err := cfg.SetConfigField(config.ScopeGlobal, fmt.Sprintf("mcp.%s", name), mcpCfg); err != nil {
		slog.Warn("Failed to persist new MCP server", "name", name, "err", err)
	}

	return nil
}

// RemoveServer removes an MCP server, closes its session, and removes it from config.
// External servers (from .mcp.json) cannot be removed — only disabled.
func RemoveServer(cfg *config.ConfigStore, name string) error {
	c := cfg.Config()
	mcpCfg, exists := c.MCP[name]
	if !exists {
		return fmt.Errorf("MCP server %q not found", name)
	}
	if mcpCfg.Source == config.MCPSourceExternal {
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be removed (disable it instead)", name)
	}

	// Close session
	if sess, ok := sessions.Get(name); ok {
		_ = sess.Close()
		sessions.Del(name)
	}
	allTools.Del(name)

	// Remove from in-memory config
	delete(c.MCP, name)

	// Remove from states and broadcast deletion
	states.Del(name)
	publishEvent(pubsub.DeletedEvent, Event{
		Type:  EventStateChanged,
		Name:  name,
		State: StateDisabled,
	})

	// Remove from persisted config
	if err := cfg.RemoveConfigField(config.ScopeGlobal, fmt.Sprintf("mcp.%s", name)); err != nil {
		slog.Warn("Failed to remove MCP from config", "name", name, "err", err)
	}

	return nil
}

func getOrRenewClient(ctx context.Context, cfg *config.ConfigStore, name string) (*ClientSession, error) {
	o := currentOwner()
	var generation uint64
	if o != nil {
		if !o.beginInit() {
			return nil, ErrOwnerBusy
		}
		defer o.endInit()
		generation = o.generation
		var stop func()
		ctx, stop = o.operationContext(ctx)
		defer stop()
	}

	sess, ok := sessions.Get(name)
	if !ok {
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}

	m := cfg.Config().MCP[name]
	state, _ := states.Get(name)

	timeout := mcpTimeout(m)
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := sess.Ping(pingCtx, nil)
	if err == nil {
		return sess, nil
	}
	updateState(name, StateError, maybeTimeoutErr(err, timeout), nil, state.Counts)
	_ = sess.Close()

	sess, err = createSession(ctx, name, m, cfg.Resolver())
	if err != nil {
		return nil, err
	}

	if o != nil {
		if !o.commitRenewal(generation, name, sess, state.Counts) {
			_ = sess.Close()
			return nil, ErrOwnerBusy
		}
		return sess, nil
	}

	updateState(name, StateConnected, nil, sess, state.Counts)
	sessions.Set(name, sess)
	return sess, nil
}

// updateState updates the state of an MCP client and publishes an event
func updateState(name string, state State, err error, client *ClientSession, counts Counts) {
	info := ClientInfo{
		Name:   name,
		State:  state,
		Error:  err,
		Client: client,
		Counts: counts,
	}
	switch state {
	case StateConnected:
		info.ConnectedAt = time.Now()
	case StateError:
		sessions.Del(name)
	}
	states.Set(name, info)

	// Publish state change event
	publishEvent(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
}

func createSession(ctx context.Context, name string, m config.MCPConfig, resolver config.VariableResolver) (*ClientSession, error) {
	timeout := mcpTimeout(m)
	mcpCtx, cancel := context.WithCancel(ctx)
	cancelTimer := time.AfterFunc(timeout, cancel)

	transport, err := createTransport(mcpCtx, m, resolver)
	if err != nil {
		updateState(name, StateError, err, nil, Counts{})
		slog.Error("Error creating MCP client", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "rush",
			Version: version.Version,
			Title:   "Rush",
		},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				publishEvent(pubsub.UpdatedEvent, Event{
					Type: EventToolsListChanged,
					Name: name,
				})
			},
			PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
				publishEvent(pubsub.UpdatedEvent, Event{
					Type: EventPromptsListChanged,
					Name: name,
				})
			},
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				publishEvent(pubsub.UpdatedEvent, Event{
					Type: EventResourcesListChanged,
					Name: name,
				})
			},
			LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
				level := parseLevel(req.Params.Level)
				slog.Log(ctx, level, "MCP log", "name", name, "logger", req.Params.Logger, "data", req.Params.Data)
			},
		},
	)

	session, err := client.Connect(mcpCtx, transport, nil)
	if err != nil {
		err = maybeStdioErr(err, transport)
		updateState(name, StateError, maybeTimeoutErr(err, timeout), nil, Counts{})
		slog.Error("MCP client failed to initialize", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	cancelTimer.Stop()
	slog.Debug("MCP client initialized", "name", name)
	return &ClientSession{session, cancel}, nil
}

// maybeStdioErr if a stdio mcp prints an error in non-json format, it'll fail
// to parse, and the cli will then close it, causing the EOF error.
// so, if we got an EOF err, and the transport is STDIO, we try to exec it
// again with a timeout and collect the output so we can add details to the
// error.
// this happens particularly when starting things with npx, e.g. if node can't
// be found or some other error like that.
func maybeStdioErr(err error, transport mcp.Transport) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	ct, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
}

func maybeTimeoutErr(err error, timeout time.Duration) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

func createTransport(ctx context.Context, m config.MCPConfig, resolver config.VariableResolver) (mcp.Transport, error) {
	switch m.Type {
	case config.MCPStdio:
		command, err := resolver.ResolveValue(m.Command)
		if err != nil {
			return nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		args, err := m.ResolvedArgs(resolver)
		if err != nil {
			return nil, err
		}
		envs, err := m.ResolvedEnv(resolver)
		if err != nil {
			return nil, err
		}
		cmd := platform.Command(ctx, home.Long(command), args...)
		cmd.Env = append(os.Environ(), envs...)
		// Run the child in its own process group and kill the whole group when
		// the session context is cancelled. A stdio server often spawns its own
		// children (signal-mcp launches signal-cli); os/exec's default
		// cancellation kills only the direct child, orphaning the rest with
		// PPID 1 — production accumulated 15+ such zombies over two days.
		configureStdioProcess(cmd)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil
	case config.MCPHttp:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp http config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers: headers,
				ctx:     ctx,
			},
		}
		return &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	case config.MCPSSE:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp sse config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers: headers,
				ctx:     ctx,
			},
		}
		return &mcp.SSEClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

type headerRoundTripper struct {
	headers map[string]string
	ctx     context.Context
}

type ownerResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
	once   sync.Once
}

func (b *ownerResponseBody) release() {
	b.once.Do(func() {
		b.stop()
		b.cancel()
	})
}

func (b *ownerResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *ownerResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	if rt.ctx != nil {
		ctx, cancel := context.WithCancel(req.Context())
		stop := context.AfterFunc(rt.ctx, cancel)
		req = req.WithContext(ctx)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			stop()
			cancel()
			return nil, err
		}
		if resp.Body == nil {
			stop()
			cancel()
			return resp, nil
		}
		resp.Body = &ownerResponseBody{
			ReadCloser: resp.Body,
			stop:       stop,
			cancel:     cancel,
		}
		return resp, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

func mcpTimeout(m config.MCPConfig) time.Duration {
	return time.Duration(cmp.Or(m.Timeout, 15)) * time.Second
}

func stdioCheck(old *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	cmd := platform.Command(ctx, old.Path, old.Args...)
	cmd.Env = old.Env
	out, err := cmd.CombinedOutput()
	if err == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: %s", err, string(out))
}
