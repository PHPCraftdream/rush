package server

import (
	"errors"

	"github.com/PHPCraftdream/rush/internal/config"
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

func AuthorizeCommand(p Principal, cmd string, policy CommandPolicy) error {
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
