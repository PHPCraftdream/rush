package sesstypes

import "time"

// RunAllowlistSpec is a JSON-serializable mirror of permission.RunAllowlistSpec.
type RunAllowlistSpec struct {
	Restrict   bool     `json:"restrict,omitempty"`
	AllowTools []string `json:"allow_tools,omitempty"`
	AllowBash  []string `json:"allow_bash,omitempty"`
}

// FileOp is the session-mirror of permission.FileOp.
type FileOp string

// FolderScopeEntry is the session-mirror of permission.FolderScopeEntry.
type FolderScopeEntry struct {
	Dir string   `json:"dir"`
	Ops []FileOp `json:"ops,omitempty"`
}

// FolderScopeSpec is a JSON-serializable mirror of permission.FolderScopeSpec.
type FolderScopeSpec struct {
	WorkingDir       string             `json:"working_dir,omitempty"`
	Entries          []FolderScopeEntry `json:"entries,omitempty"`
	KeepCommandTools bool               `json:"keep_command_tools,omitempty"`
}

// CallOptionsSpecVersion is the current schema version.
const CallOptionsSpecVersion = 3

// CallOptionsSpec is a JSON-serializable mirror of the replay-relevant subset of agent.CallOptions.
type CallOptionsSpec struct {
	Version                  int           `json:"version"`
	DisableSubAgents         bool          `json:"disable_sub_agents,omitempty"`
	ModelRole                string        `json:"model_role,omitempty"`
	TimeoutOptionsSet        bool          `json:"timeout_options_set,omitempty"`
	TimeoutExtendsOnProgress bool          `json:"timeout_extends_on_progress,omitempty"`
	TimeoutHardCap           time.Duration `json:"timeout_hard_cap,omitempty"`
	IdleTimeout              time.Duration `json:"idle_timeout,omitempty"`
	WebTurnOptIn             bool          `json:"web_turn_opt_in,omitempty"`
	MaxAgentDepth            int           `json:"max_agent_depth,omitempty"`
	AgentDepthSet            bool          `json:"agent_depth_set,omitempty"`
	RoleContract             *RoleContract `json:"role_contract,omitempty"`
}

// SessionExecutionRequirement is the durable execution requirement of one session.
type SessionExecutionRequirement struct {
	BindingID        string            `json:"binding_id"`
	Owner            string            `json:"owner,omitempty"`
	Epoch            int64             `json:"epoch,omitempty"`
	FolderScopeSpec  *FolderScopeSpec  `json:"folder_scope_spec,omitempty"`
	RunAllowlistSpec *RunAllowlistSpec `json:"run_allowlist_spec,omitempty"`
	CallPolicy       *CallOptionsSpec  `json:"call_policy,omitempty"`
}

// SessionCredentialMarkerRowID returns the reserved metadata row ID.
func SessionCredentialMarkerRowID(sessionID string) string {
	return "session-credential-requirement:" + sessionID
}

// SessionExecutionRequirementRowID returns the deterministic reserved row ID.
func SessionExecutionRequirementRowID(sessionID string) string {
	return "session-execution-requirement:" + sessionID
}
