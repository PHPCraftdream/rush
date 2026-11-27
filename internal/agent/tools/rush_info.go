package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt/skills"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

const RushInfoToolName = "rush_info"

//go:embed rush_info.md
var rushInfoDescription string

type RushInfoParams struct{}

func NewRushInfoTool(
	cfg *config.ConfigStore,
	allSkills []*skills.Skill,
	activeSkills []*skills.Skill,
	skillTracker *skills.Tracker,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RushInfoToolName,
		rushInfoDescription,
		func(ctx context.Context, _ RushInfoParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(buildRushInfo(cfg, allSkills, activeSkills, skillTracker)), nil
		},
	)
}

func buildRushInfo(cfg *config.ConfigStore, allSkills []*skills.Skill, activeSkills []*skills.Skill, skillTracker *skills.Tracker) string {
	// The mcp calls below are read-only global-map getters; isolation comes
	// from the IsConfigured filtering inside writeMCP; there is no owner
	// dimension until the registry maps are per-owner.
	var b strings.Builder

	writeConfigFiles(&b, cfg)
	writeConfigStaleness(&b, cfg)
	writeModels(&b, cfg)
	writeProviders(&b, cfg)
	writeMCP(&b, mcp.GetStates(), cfg)
	writeSkills(&b, allSkills, activeSkills, skillTracker, cfg)
	writeHooks(&b, cfg)
	writePermissions(&b, cfg)
	writeDisabledTools(&b, cfg)
	writeOptions(&b, cfg)
	writeAttribution(&b, cfg)

	return b.String()
}

func writeConfigFiles(b *strings.Builder, cfg *config.ConfigStore) {
	b.WriteString("[config_files]\n")
	paths := cfg.LoadedPaths()
	for _, p := range paths {
		b.WriteString(p + "\n")
	}
	b.WriteString("\n")
}

func writeConfigStaleness(b *strings.Builder, cfg *config.ConfigStore) {
	staleness := cfg.ConfigStaleness()

	b.WriteString("[config]\n")
	fmt.Fprintf(b, "dirty = %v\n", staleness.Dirty)

	if len(staleness.Changed) > 0 {
		sorted := slices.Clone(staleness.Changed)
		slices.Sort(sorted)
		fmt.Fprintf(b, "changed_paths = %s\n", strings.Join(sorted, ", "))
	}

	if len(staleness.Missing) > 0 {
		sorted := slices.Clone(staleness.Missing)
		slices.Sort(sorted)
		fmt.Fprintf(b, "missing_paths = %s\n", strings.Join(sorted, ", "))
	}

	if len(staleness.Errors) > 0 {
		var paths []string
		for path := range staleness.Errors {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		fmt.Fprintf(b, "errors = %s\n", strings.Join(paths, ", "))
	}

	b.WriteString("\n")
}

// writeModels prints the [model] section, one line per configured slot.
// Rush resolves models through four named slots: "smart" (the strong
// top-level default) and "fast" (cheap trivial work) are always
// meaningful; "worker" (cheap sub-task delegation target) and "reviewer"
// (strongest slot, explicit --role reviewer only) are optional — they are
// skipped below via `if !ok { continue }` when unconfigured, so their
// absence from the dump means "not set", not "broken".
func writeModels(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	if len(c.Models) == 0 {
		return
	}
	b.WriteString("[model]\n")
	for _, typ := range []config.SelectedModelType{
		config.SelectedModelTypeSmart,
		config.SelectedModelTypeFast,
		config.SelectedModelTypeWorker,
		config.SelectedModelTypeReviewer,
	} {
		m, ok := c.Models[typ]
		if !ok {
			continue
		}
		fmt.Fprintf(b, "%s = %s (%s)\n", typ, m.Model, m.Provider)
	}
	b.WriteString("\n")
}

func writeProviders(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	type pv struct {
		name  string
		count int
	}
	var providers []pv
	for name, pc := range c.Providers.Seq2() {
		if pc.Disable {
			continue
		}
		providers = append(providers, pv{name: name, count: len(pc.Models)})
	}
	if len(providers) == 0 {
		return
	}
	slices.SortFunc(providers, func(a, b pv) int { return strings.Compare(a.name, b.name) })
	b.WriteString("[providers]\n")
	for _, p := range providers {
		fmt.Fprintf(b, "%s = enabled (%d models)\n", p.name, p.count)
	}
	b.WriteString("\n")
}

func writeMCP(b *strings.Builder, states map[string]mcp.ClientInfo, cfg *config.ConfigStore) {
	// Write runtime MCP states
	if len(states) > 0 {
		type entry struct {
			name        string
			state       mcp.State
			err         error
			tools       int
			resources   int
			connectedAt string
		}
		var entries []entry
		for name, info := range states {
			if !mcp.IsConfigured(cfg, name) {
				continue
			}
			e := entry{
				name:  name,
				state: info.State,
				err:   info.Error,
			}
			if info.State == mcp.StateConnected {
				e.tools = info.Counts.Tools
				e.resources = info.Counts.Resources
				if !info.ConnectedAt.IsZero() {
					e.connectedAt = info.ConnectedAt.Format("15:04:05")
				}
			}
			entries = append(entries, e)
		}
		slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
		if len(entries) > 0 {
			b.WriteString("[mcp]\n")
			for _, e := range entries {
				switch e.state {
				case mcp.StateConnected:
					if e.connectedAt != "" {
						fmt.Fprintf(b, "%s = connected (%d tools, %d resources) since %s\n", e.name, e.tools, e.resources, e.connectedAt)
					} else {
						fmt.Fprintf(b, "%s = connected (%d tools, %d resources)\n", e.name, e.tools, e.resources)
					}
				case mcp.StateError:
					if e.err != nil {
						fmt.Fprintf(b, "%s = error: %s\n", e.name, e.err.Error())
					} else {
						fmt.Fprintf(b, "%s = error\n", e.name)
					}
				default:
					fmt.Fprintf(b, "%s = %s\n", e.name, e.state)
				}
			}
			b.WriteString("\n")
		}
	}

	// Write configured but not running MCP servers
	c := cfg.Config()
	if len(c.MCP) > 0 {
		runtimeNames := make(map[string]bool)
		for name := range states {
			if mcp.IsConfigured(cfg, name) {
				runtimeNames[name] = true
			}
		}

		type configuredEntry struct {
			name   string
			status string
		}
		var entries []configuredEntry
		for name, mcpCfg := range c.MCP {
			// Skip if already in runtime
			if runtimeNames[name] {
				continue
			}
			status := "not_started"
			if mcpCfg.Disabled {
				status = "disabled"
			}
			entries = append(entries, configuredEntry{name: name, status: status})
		}

		if len(entries) > 0 {
			slices.SortFunc(entries, func(a, b configuredEntry) int { return strings.Compare(a.name, b.name) })
			b.WriteString("[mcp_configured]\n")
			for _, e := range entries {
				fmt.Fprintf(b, "%s = %s\n", e.name, e.status)
			}
			b.WriteString("\n")
		}
	}
}

func writeSkills(b *strings.Builder, allSkills []*skills.Skill, activeSkills []*skills.Skill, tracker *skills.Tracker, cfg *config.ConfigStore) {
	var disabled []string
	if cfg.Config().Options != nil {
		disabled = cfg.Config().Options.DisabledSkills
	}
	if len(activeSkills) == 0 && len(disabled) == 0 {
		return
	}

	// Build origin map from the pre-filter list.
	originMap := make(map[string]string, len(allSkills))
	for _, s := range allSkills {
		if s.Builtin {
			originMap[s.Name] = "builtin"
		} else {
			originMap[s.Name] = "user"
		}
	}

	type entry struct {
		name   string
		origin string
		state  string
	}
	var entries []entry

	// Active skills: loaded or unloaded.
	for _, s := range activeSkills {
		state := "unloaded"
		if tracker.IsLoaded(s.Name) {
			state = "loaded"
		}
		origin := originMap[s.Name]
		entries = append(entries, entry{name: s.Name, origin: origin, state: state})
	}

	// Disabled skills.
	for _, name := range disabled {
		origin := originMap[name]
		if origin == "" {
			origin = "user"
		}
		entries = append(entries, entry{name: name, origin: origin, state: "disabled"})
	}

	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	b.WriteString("[skills]\n")
	fmt.Fprintf(b, "loaded_this_session = %d/%d\n", tracker.LoadedCount(), len(activeSkills))
	for _, e := range entries {
		fmt.Fprintf(b, "%s = %s, %s\n", e.name, e.origin, e.state)
	}
	b.WriteString("\n")
}

func writePermissions(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	overrides := cfg.Overrides()

	if c.Permissions == nil {
		if !overrides.SkipPermissionRequests {
			return
		}
	} else if !overrides.SkipPermissionRequests && len(c.Permissions.AllowedTools) == 0 {
		return
	}
	b.WriteString("[permissions]\n")
	if overrides.SkipPermissionRequests {
		b.WriteString("mode = yolo\n")
	}
	if c.Permissions != nil && len(c.Permissions.AllowedTools) > 0 {
		sorted := slices.Clone(c.Permissions.AllowedTools)
		slices.Sort(sorted)
		fmt.Fprintf(b, "allowed_tools = %s\n", strings.Join(sorted, ", "))
	}
	b.WriteString("\n")
}

func writeDisabledTools(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	if c.Options == nil || len(c.Options.DisabledTools) == 0 {
		return
	}
	sorted := slices.Clone(c.Options.DisabledTools)
	slices.Sort(sorted)
	b.WriteString("[tools]\n")
	fmt.Fprintf(b, "disabled = %s\n", strings.Join(sorted, ", "))
	b.WriteString("\n")
}

func writeOptions(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	if c.Options == nil {
		return
	}
	type kv struct {
		key   string
		value string
	}
	var opts []kv

	opts = append(opts, kv{"data_directory", c.Options.DataDirectory})
	opts = append(opts, kv{"debug", fmt.Sprintf("%v", c.Options.Debug)})
	autoSummarize := !c.Options.DisableAutoSummarize
	opts = append(opts, kv{"auto_summarize", fmt.Sprintf("%v", autoSummarize)})

	slices.SortFunc(opts, func(a, b kv) int { return strings.Compare(a.key, b.key) })
	b.WriteString("[options]\n")
	for _, o := range opts {
		fmt.Fprintf(b, "%s = %s\n", o.key, o.value)
	}
	b.WriteString("\n")
}

func writeAttribution(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	if c.Options == nil || c.Options.Attribution == nil {
		return
	}
	b.WriteString("[attribution]\n")
	trailerStyle := c.Options.Attribution.TrailerStyle
	if trailerStyle == "" {
		trailerStyle = config.TrailerStyleCoAuthoredBy
	}
	fmt.Fprintf(b, "trailer_style = %s\n", trailerStyle)
	fmt.Fprintf(b, "generated_with = %v\n", c.Options.Attribution.GeneratedWith)
	b.WriteString("\n")
}

func writeHooks(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	if len(c.Hooks) == 0 {
		return
	}

	type entry struct {
		event   string
		matcher string
		command string
		timeout int
	}
	var entries []entry
	for event, hookList := range c.Hooks {
		for _, h := range hookList {
			entries = append(entries, entry{
				event:   event,
				matcher: h.Matcher,
				command: h.Command,
				timeout: h.Timeout,
			})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.event != b.event {
			return strings.Compare(a.event, b.event)
		}
		return strings.Compare(a.command, b.command)
	})

	b.WriteString("[hooks]\n")
	for _, e := range entries {
		line := fmt.Sprintf("%s = %s", e.event, e.command)
		if e.matcher != "" {
			line = fmt.Sprintf("%s (matcher: %s) = %s", e.event, e.matcher, e.command)
		}
		if e.timeout > 0 && e.timeout != 30 {
			line += fmt.Sprintf(" (timeout: %ds)", e.timeout)
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n")
}

//go:embed todos.md
var todosDescription string

const TodosToolName = "todos"

type TodosParams struct {
	Todos []TodoItem `json:"todos" description:"The updated todo list"`
}

type TodoItem struct {
	Content string `json:"content" description:"What needs to be done (imperative form)"`
	// The enum tag reaches the JSON schema handed to the provider (see
	// fantasy's schema.Generate), so a model is told which values are legal
	// instead of having to infer them from the description. That is defence
	// in depth, not the fix: schema enforcement varies by provider, so the
	// runtime check in NewTodosTool still has to hold — and it answers with a
	// correctable tool error rather than killing the run.
	Status     string `json:"status" description:"Task status: pending, in_progress, or completed" enum:"pending,in_progress,completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g., 'Running tests')"`
}

type TodosResponseMetadata struct {
	IsNew         bool           `json:"is_new"`
	Todos         []session.Todo `json:"todos"`
	JustCompleted []string       `json:"just_completed,omitempty"`
	JustStarted   string         `json:"just_started,omitempty"`
	Completed     int            `json:"completed"`
	Total         int            `json:"total"`
}

// todoStatusLevel returns a numeric rank for a todo status so we can
// enforce forward-only progression: pending(0) → in_progress(1) → completed(2).
func todoStatusLevel(s session.TodoStatus) int {
	switch s {
	case session.TodoStatusInProgress:
		return 1
	case session.TodoStatusCompleted:
		return 2
	default:
		return 0
	}
}

// mergeTodos merges the model's desired todo list with the current DB state.
//
// Two protective rules apply:
//  1. Status protection: a task's status can only advance
//     (pending → in_progress → completed). The model cannot revert a status
//     the operator manually set.
//  2. Tombstone filtering: if the operator explicitly deleted a todo (its
//     Content string appears in deletedTombstones), the model cannot
//     resurrect it — the item is silently dropped from the result even if
//     the model sends it again.
//
// The model's list is otherwise authoritative — if the model omits a task,
// it is removed.
func mergeTodos(dbTodos []session.Todo, modelItems []TodoItem, deletedTombstones []string) ([]session.Todo, bool) {
	// Build the tombstone set for O(1) lookup.
	tombstoneSet := make(map[string]struct{}, len(deletedTombstones))
	for _, c := range deletedTombstones {
		tombstoneSet[c] = struct{}{}
	}

	if len(dbTodos) == 0 {
		// Empty DB → accept model's list as-is (fresh start), but still
		// honour tombstones so a model that re-sends deleted items on its
		// very first call after deletion is also filtered.
		var todos []session.Todo
		for _, item := range modelItems {
			if _, tombstoned := tombstoneSet[item.Content]; tombstoned {
				slog.Info("todos tool: filtered tombstoned todo (fresh start)",
					"content", item.Content,
				)
				continue
			}
			todos = append(todos, session.Todo{
				Content:    item.Content,
				Status:     session.TodoStatus(item.Status),
				ActiveForm: item.ActiveForm,
			})
		}
		return todos, true
	}

	dbByContent := make(map[string]session.Todo, len(dbTodos))
	for _, t := range dbTodos {
		dbByContent[t.Content] = t
	}

	var result []session.Todo

	// Process model's items: apply tombstone filter then status protection.
	for _, item := range modelItems {
		if _, tombstoned := tombstoneSet[item.Content]; tombstoned {
			slog.Info("todos tool: filtered tombstoned todo",
				"content", item.Content,
			)
			continue
		}
		wantStatus := session.TodoStatus(item.Status)
		if dbTodo, exists := dbByContent[item.Content]; exists {
			// Task exists in DB: don't allow status regression.
			if todoStatusLevel(dbTodo.Status) > todoStatusLevel(wantStatus) {
				slog.Info("todos tool: protecting status from regression",
					"content", item.Content,
					"db_status", dbTodo.Status,
					"model_status", wantStatus,
				)
				wantStatus = dbTodo.Status
			}
		}
		result = append(result, session.Todo{
			Content:    item.Content,
			Status:     wantStatus,
			ActiveForm: item.ActiveForm,
		})
	}

	return result, false
}

func NewTodosTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TodosToolName,
		todosDescription,
		func(ctx context.Context, params TodosParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for managing todos")
			}

			currentSession, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}

			// A bad status is the model's mistake to correct, so it comes back
			// as a tool-error RESPONSE, not as a returned error. fantasy
			// treats the two very differently: a returned error sets
			// executeSingleTool's isCriticalError flag and executeTools then
			// aborts the whole agent loop, ending the run outright — one
			// mistyped enum value would kill a session that had been working
			// for many minutes, and the model would never see why. A response
			// carrying IsError is fed back as an ordinary tool result it can
			// react to.
			//
			// The three returned errors elsewhere in this function stay as
			// they are: a missing session ID or a failed DB read/write is not
			// something the model can retry its way out of.
			//
			// The message names the offending item and spells out the legal
			// values, because the likeliest way to get here is omitting the
			// field entirely (which unmarshals to "") rather than deliberately
			// choosing a wrong value.
			for _, item := range params.Todos {
				switch item.Status {
				case "pending", "in_progress", "completed":
				default:
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"invalid status %q for todo %q: status must be exactly one of "+
							`"pending", "in_progress" or "completed". No todos were saved; `+
							"resend the whole list with a valid status on every item.",
						item.Status, item.Content,
					)), nil
				}
			}

			todos, isNew := mergeTodos(currentSession.Todos, params.Todos, currentSession.DeletedTodos)

			slog.Info("todos tool: model updating todos",
				"session", sessionID,
				"prev", currentSession.Todos,
				"merged", todos,
			)

			// Compute response metadata.
			oldStatusByContent := make(map[string]session.TodoStatus)
			for _, todo := range currentSession.Todos {
				oldStatusByContent[todo.Content] = todo.Status
			}
			var justCompleted []string
			var justStarted string
			completedCount, pendingCount, inProgressCount := 0, 0, 0

			for _, todo := range todos {
				switch todo.Status {
				case session.TodoStatusCompleted:
					completedCount++
					if old, existed := oldStatusByContent[todo.Content]; existed && old != session.TodoStatusCompleted {
						justCompleted = append(justCompleted, todo.Content)
					}
				case session.TodoStatusInProgress:
					inProgressCount++
					if old, existed := oldStatusByContent[todo.Content]; !existed || old != session.TodoStatusInProgress {
						if todo.ActiveForm != "" {
							justStarted = todo.ActiveForm
						} else {
							justStarted = todo.Content
						}
					}
				default:
					pendingCount++
				}
			}

			if err := sessions.SetTodos(ctx, sessionID, todos, currentSession.DeletedTodos); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save todos: %w", err)
			}

			response := fmt.Sprintf("Todo list updated successfully.\nStatus: %d pending, %d in progress, %d completed\n",
				pendingCount, inProgressCount, completedCount)
			response += "Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable."

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), TodosResponseMetadata{
				IsNew:         isNew,
				Todos:         todos,
				JustCompleted: justCompleted,
				JustStarted:   justStarted,
				Completed:     completedCount,
				Total:         len(todos),
			}), nil
		})
}
