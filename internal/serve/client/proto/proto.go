package proto

import (
	"encoding/json"
	"errors"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
)

// Workspace represents a running app.App workspace with its associated
// resources and state.
type Workspace struct {
	ID      string         `json:"id"`
	Path    string         `json:"path"`
	YOLO    bool           `json:"yolo,omitempty"`
	Debug   bool           `json:"debug,omitempty"`
	DataDir string         `json:"data_dir,omitempty"`
	Version string         `json:"version,omitempty"`
	Config  *config.Config `json:"config,omitempty"`
	Env     []string       `json:"env,omitempty"`
	// Fork merge note: upstream's `Skills []SkillState` field carried a
	// snapshot of skill discovery state to their TUI clients over their
	// REST API. Dropped — we rejected the SkillState wire type
	// (proto/skills.go) along with the rest of their multi-client skills
	// architecture. See CHANGELOG.fork.md Section 2.
}

// Error represents an error response.
type Error struct {
	Message string `json:"message"`
}

// SkillInfo describes a visible skill exposed to a frontend.
type SkillInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Label       string `json:"label"`
	Source      string `json:"source"`
}

// ReadSkillRequest is the request body for reading a skill's content.
type ReadSkillRequest struct {
	SkillID string `json:"skill_id"`
}

// ReadSkillResponse is the response for reading a skill's content.
type ReadSkillResponse struct {
	Content []byte          `json:"content"`
	Result  SkillReadResult `json:"result"`
}

// SkillReadResult holds metadata about a skill returned alongside its
// content.
type SkillReadResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Builtin     bool   `json:"builtin"`
}

// AgentInfo represents information about the agent.
type AgentInfo struct {
	IsBusy   bool                 `json:"is_busy"`
	IsReady  bool                 `json:"is_ready"`
	Model    catwalk.Model        `json:"model"`
	ModelCfg config.SelectedModel `json:"model_cfg"`
}

// IsZero checks if the AgentInfo is zero-valued.
func (a AgentInfo) IsZero() bool {
	return !a.IsBusy && !a.IsReady && a.Model.ID == ""
}

// AgentMessage represents a message sent to the agent.
type AgentMessage struct {
	SessionID   string       `json:"session_id"`
	Prompt      string       `json:"prompt"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// AgentSession represents a session with its busy status.
type AgentSession struct {
	Session
	IsBusy bool `json:"is_busy"`
}

// IsZero checks if the AgentSession is zero-valued.
func (a AgentSession) IsZero() bool {
	return a.ID == "" && !a.IsBusy
}

// PermissionAction represents an action taken on a permission request.
type PermissionAction string

const (
	PermissionAllow           PermissionAction = "allow"
	PermissionAllowForSession PermissionAction = "allow_session"
	PermissionDeny            PermissionAction = "deny"
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (p PermissionAction) MarshalText() ([]byte, error) {
	return []byte(p), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (p *PermissionAction) UnmarshalText(text []byte) error {
	*p = PermissionAction(text)
	return nil
}

// PermissionGrant represents a permission grant request.
type PermissionGrant struct {
	Permission PermissionRequest `json:"permission"`
	Action     PermissionAction  `json:"action"`
}

// PermissionSkipRequest represents a request to skip permission prompts.
type PermissionSkipRequest struct {
	Skip bool `json:"skip"`
}

// AgentEventType represents the type of agent event.
type AgentEventType string

const (
	AgentEventTypeError     AgentEventType = "error"
	AgentEventTypeResponse  AgentEventType = "response"
	AgentEventTypeSummarize AgentEventType = "summarize"
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (t AgentEventType) MarshalText() ([]byte, error) {
	return []byte(t), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (t *AgentEventType) UnmarshalText(text []byte) error {
	*t = AgentEventType(text)
	return nil
}

// AgentEvent represents an event emitted by the agent.
type AgentEvent struct {
	Type    AgentEventType `json:"type"`
	Message Message        `json:"message"`
	Error   error          `json:"error,omitempty"`

	// When summarizing.
	SessionID    string `json:"session_id,omitempty"`
	SessionTitle string `json:"session_title,omitempty"`
	Progress     string `json:"progress,omitempty"`
	Done         bool   `json:"done,omitempty"`
}

// MarshalJSON implements the [json.Marshaler] interface.
func (e AgentEvent) MarshalJSON() ([]byte, error) {
	type Alias AgentEvent
	return json.Marshal(&struct {
		Error string `json:"error,omitempty"`
		Alias
	}{
		Error: func() string {
			if e.Error != nil {
				return e.Error.Error()
			}
			return ""
		}(),
		Alias: Alias(e),
	})
}

// UnmarshalJSON implements the [json.Unmarshaler] interface.
func (e *AgentEvent) UnmarshalJSON(data []byte) error {
	type Alias AgentEvent
	aux := &struct {
		Error string `json:"error,omitempty"`
		Alias
	}{
		Alias: Alias(*e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*e = AgentEvent(aux.Alias)
	if aux.Error != "" {
		e.Error = errors.New(aux.Error)
	}
	return nil
}

// File represents a file tracked in session history.
type File struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Version   int64  `json:"version"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// ServerControl represents a server control request.
type ServerControl struct {
	Command string `json:"command"`
}

// Session represents a session in the proto layer.
type Session struct {
	ID               string  `json:"id"`
	ParentSessionID  string  `json:"parent_session_id"`
	Title            string  `json:"title"`
	MessageCount     int64   `json:"message_count"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	SummaryMessageID string  `json:"summary_message_id"`
	Cost             float64 `json:"cost"`
	Todos            []Todo  `json:"todos,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	UpdatedAt        int64   `json:"updated_at"`
}

// Todo represents a single todo entry on a session in the proto layer.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form"`
}

// VersionInfo represents version information about the server.
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildID   string `json:"build_id"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}
