package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

var commandScopes = map[string]string{
	"send_message":               "run",    // Sends a prompt that executes work.
	"interrupt_and_send":         "run",    // Replaces an active run with new work.
	"inject_message":             "run",    // Injects content into a session.
	"cancel_agent":               "run",    // Stops active work.
	"create_session":             "run",    // Creates a workspace session.
	"fork_session":               "run",    // Creates a session fork.
	"delete_session":             "run",    // Deletes session data.
	"delete_other_sessions":      "run",    // Deletes sessions beyond the current one.
	"list_sessions":              "read",   // Lists sessions.
	"load_messages":              "read",   // Reads session messages.
	"get_config":                 "read",   // Reads effective configuration.
	"get_logs":                   "admin",  // May disclose every session's prompts.
	"set_theme":                  "config", // Changes UI configuration.
	"set_keep_alive":             "config", // Changes session behavior.
	"rename_session":             "run",    // Mutates a session.
	"set_session_models":         "config", // Changes model settings.
	"remove_recent_model":        "run",    // Changes per-session recent model state.
	"track_model_usage":          "run",    // Records model use for session work.
	"delete_message":             "run",    // Deletes a message.
	"delete_messages":            "run",    // Deletes messages.
	"update_message_content":     "run",    // Edits a message.
	"update_message_thinking":    "run",    // Edits private reasoning data.
	"get_system_prompt":          "read",   // Reads prompt configuration.
	"set_system_prompt":          "config", // Changes prompt configuration.
	"summarize_session":          "run",    // Starts session work.
	"cancel_queued_summarize":    "run",    // Cancels queued work.
	"delete_message_part":        "run",    // Deletes message content.
	"update_message_part":        "run",    // Edits message content.
	"toggle_pin_message":         "run",    // Mutates a session message.
	"rerun_message":              "run",    // Re-executes prior work.
	"get_custom_roles":           "read",   // Reads role configuration.
	"set_custom_role":            "config", // Changes a custom role.
	"remove_custom_role":         "config", // Removes a custom role.
	"get_scoped_models":          "read",   // Reads model configuration.
	"set_scoped_model":           "config", // Changes model configuration.
	"clear_scoped_model":         "config", // Clears model configuration.
	"get_session_live_work":      "read",   // Reads live execution status.
	"get_session_wake_schedules": "read",   // Reads scheduled work.
	"get_session_batch_work":     "read",   // Reads batch execution status.
	"batch_status":               "read",   // Reads batch status.
	"batch_output":               "read",   // Reads batch output.
	"batch_stop":                 "run",    // Stops batch work.
	"batch_add":                  "run",    // Adds batch work.
	"batch_drain":                "run",    // Drains batch work.
	"cancel_wake_schedule":       "run",    // Cancels scheduled work.
	"shutdown_server":            "admin",  // Shuts down the service.
	"set_debug":                  "config", // Changes diagnostic configuration.
	"remove_context_path":        "admin",  // Removes an exposed filesystem path.
	"get_skills":                 "read",   // Reads available skills.
	"add_skills_path":            "admin",  // Adds a filesystem path for skills.
	"remove_skills_path":         "admin",  // Removes a filesystem path for skills.
	"remove_custom_provider":     "admin",  // Removes provider configuration.
	"set_provider_peak_hours":    "admin",  // Changes provider operating policy.
	"pause_provider_peak_hours":  "admin",  // Pauses provider operating policy.
	"resume_provider_peak_hours": "admin",  // Resumes provider operating policy.
	"get_peak_hours_status":      "admin",  // Reads provider operating policy and status.
	"update_todos":               "run",    // Updates task state.
	"log_client_event":           "run",    // Records a client event.
	"log_client_error":           "run",    // Records a client error.
	"set_mcp_disabled":           "admin",  // Changes executable MCP availability.
	"remove_mcp_server":          "admin",  // Removes an executable MCP server.
	"set_lsp_disabled":           "admin",  // Changes executable LSP availability.
	"add_lsp_server":             "admin",  // Adds an executable LSP server.
	"remove_lsp_server":          "admin",  // Removes an executable LSP server.
	"update_lsp_server":          "admin",  // Changes an executable LSP server.
	"set_provider_key":           "admin",  // Exposes provider credentials.
	"remove_provider_key":        "admin",  // Removes provider credentials.
	"add_mcp_server":             "admin",  // Adds an executable MCP server.
	"update_mcp_server":          "admin",  // Changes an executable MCP server.
	"add_context_path":           "admin",  // Exposes filesystem content.
	"initialize_project":         "admin",  // Initializes workspace state.
	"add_custom_provider":        "admin",  // Adds provider configuration.
	"update_custom_provider":     "admin",  // Changes provider configuration.
}

var (
	ErrCommandUnmapped = errors.New("unmapped command")
	ErrAdminDisallowed = errors.New("admin command disallowed on this listener")
	ErrScopeForbidden  = errors.New("command scope forbidden")
)

func ScopeFor(cmd string) (string, bool) { s, ok := commandScopes[cmd]; return s, ok }

type CommandPolicy struct{ AllowAdmin bool }

func AuthorizeCommand(p apiauth.Principal, cmd string, policy CommandPolicy) error {
	scope, ok := ScopeFor(cmd)
	if !ok {
		return ErrCommandUnmapped
	}
	if scope == "admin" && !policy.AllowAdmin {
		return ErrAdminDisallowed
	}
	if !config.APITokenScopeAllows(p.Scopes, scope) {
		return ErrScopeForbidden
	}
	return nil
}

type ClientAuth struct {
	principal apiauth.Principal
	policy    CommandPolicy
	view      pinnedView
}

func NewClientAuth(principal apiauth.Principal, policy CommandPolicy) *ClientAuth {
	principal.Scopes = append([]string(nil), principal.Scopes...)
	return &ClientAuth{principal: principal, policy: policy, view: newPinnedView(principal.PinnedSession)}
}

func (c *Client) mayReceive(msgType string) bool {
	if c == nil || c.auth == nil {
		return true
	}
	return c.auth.mayReceive(msgType)
}

// mayReceive is the type-level rule: mcp_state (MCP environment and headers)
// only reaches admin clients of an allow_admin listener.
func (a *ClientAuth) mayReceive(msgType string) bool {
	return msgType != wire.EventMCPState || (a.policy.AllowAdmin && hasScope(a.principal, "admin"))
}

// replyBypassesView lets the correlated error/ok reply to a command through a
// pinned view: the command already passed the pre-handler session resolution,
// so it targeted the pinned session, and these envelopes carry no event
// payload of another session. Uncorrelated or data-bearing replies still go
// through the view filter.
func (a *ClientAuth) replyBypassesView(id, msgType string) bool {
	return a.view.sessionID != "" && id != "" && (msgType == wire.EventError || msgType == wire.EventResponse)
}

// filter returns the bytes this client may receive, or nil. The payload is
// parsed (once per delivery) only for pinned clients.
func (a *ClientAuth) filter(raw []byte, m *lazyEventMeta) []byte {
	msgType := m.typ()
	if msgType == "" || !a.mayReceive(msgType) {
		return nil
	}
	if msgType == wire.EventConfig && !config.APITokenScopeAllows(a.principal.Scopes, "config") {
		if raw = redactConfigLocalPaths(raw); raw == nil {
			return nil
		}
	}
	if a.view.sessionID == "" {
		return raw
	}
	return a.view.project(raw, m.meta())
}

func hasScope(principal apiauth.Principal, scope string) bool {
	for _, granted := range principal.Scopes {
		if granted == scope || granted == "admin" {
			return true
		}
	}
	return false
}

func authorizeWSCommand(auth ClientAuth, command string) error {
	err := AuthorizeCommand(auth.principal, command, auth.policy)
	switch {
	case errors.Is(err, ErrScopeForbidden):
		return errors.New("forbidden: scope")
	case errors.Is(err, ErrAdminDisallowed):
		return errors.New("forbidden: admin commands are disabled on this listener")
	case errors.Is(err, ErrCommandUnmapped):
		return errors.New("forbidden: unknown command")
	default:
		if err != nil {
			return fmt.Errorf("forbidden: %w", err)
		}
		return nil
	}
}

// envelopeType returns the "type" of an encoded WSMessage. Broadcast
// envelopes start with the type field, which is read without a full decode;
// anything else falls back to JSON.
func envelopeType(raw []byte) string {
	const prefix = `{"type":"`
	if bytes.HasPrefix(raw, []byte(prefix)) {
		rest := raw[len(prefix):]
		if end := bytes.IndexByte(rest, '"'); end >= 0 && bytes.IndexByte(rest[:end], '\\') < 0 {
			return string(rest[:end])
		}
	}
	var message struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return ""
	}
	return message.Type
}

// configLocalPathKeys are the config event payload fields that expose this
// machine's filesystem layout. Tokens below the config scope never see them;
// the interactive UI (no auth) and config/admin tokens do.
var configLocalPathKeys = []string{"cwd", "contextPaths", "skillsPaths"}

// redactConfigLocalPaths drops configLocalPathKeys from a config envelope. It
// fails closed: an envelope it cannot parse is dropped (nil) rather than sent
// unredacted.
func redactConfigLocalPaths(raw []byte) []byte {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	payload, ok := envelope["payload"]
	if !ok {
		return raw
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil
	}
	for _, key := range configLocalPathKeys {
		delete(fields, key)
	}
	redacted, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	envelope["payload"] = redacted
	out, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	return out
}

// AuthorizeIncoming is the S4 seam (design §3): the authorization block of
// handleIncoming (handlers.go:39-55 at HEAD) moved verbatim into this method.
// It replies with the exact denial envelopes and returns the non-zero status
// when the frame is denied, 0 when allowed. Signature delta vs design §3
// (msgType, payload): the moved block replies with msg.ID, so msgID is a
// required parameter for root callers.
func (c *Client) AuthorizeIncoming(msgID, msgType string, payload json.RawMessage) int {
	if c.auth == nil {
		return 0
	}
	if status := apiauth.ResolvePinnedCommand(c.auth.principal.PinnedSession, msgType, payload); status != 0 {
		if status == http.StatusNotFound {
			c.Reply(msgID, wire.EventError, nil, "not found")
		} else {
			c.Reply(msgID, wire.EventError, nil, "forbidden: pinned-token")
		}
		return status
	}
	if c.auth.principal.BatchOnly && apiauth.ResolveBatchOnlyCommand(msgType) != 0 {
		c.Reply(msgID, wire.EventError, nil, "forbidden: batch-only")
		return http.StatusForbidden
	}
	if err := authorizeWSCommand(*c.auth, msgType); err != nil {
		c.Reply(msgID, wire.EventError, nil, err.Error())
		return http.StatusForbidden
	}
	return 0
}

// CommandScopesSnapshot returns a copy of the command→scope table for its
// staying root consumer
// api_session_resolution_test.go:13-110 iterates the table until L6/L7. A
// copy, not the live map: §2.4 forbids exported mutable vars, and the table
// is read-only by construction.
func CommandScopesSnapshot() map[string]string {
	out := make(map[string]string, len(commandScopes))
	for k, v := range commandScopes {
		out[k] = v
	}
	return out
}
