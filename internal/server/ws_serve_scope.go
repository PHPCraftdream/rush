package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
)

type clientAuth struct {
	principal Principal
	policy    CommandPolicy
	view      pinnedView
}

func newClientAuth(principal Principal, policy CommandPolicy) *clientAuth {
	principal.Scopes = append([]string(nil), principal.Scopes...)
	return &clientAuth{principal: principal, policy: policy, view: newPinnedView(principal.PinnedSession)}
}

func (c *Client) mayReceive(msgType string) bool {
	if c == nil || c.auth == nil {
		return true
	}
	return c.auth.mayReceive(msgType)
}

// mayReceive is the type-level rule: mcp_state (MCP environment and headers)
// only reaches admin clients of an allow_admin listener.
func (a *clientAuth) mayReceive(msgType string) bool {
	return msgType != EventMCPState || (a.policy.AllowAdmin && hasScope(a.principal, "admin"))
}

// replyBypassesView lets the correlated error/ok reply to a command through a
// pinned view: the command already passed the pre-handler session resolution,
// so it targeted the pinned session, and these envelopes carry no event
// payload of another session. Uncorrelated or data-bearing replies still go
// through the view filter.
func (a *clientAuth) replyBypassesView(id, msgType string) bool {
	return a.view.sessionID != "" && id != "" && (msgType == EventError || msgType == EventResponse)
}

// filter returns the bytes this client may receive, or nil. The payload is
// parsed (once per delivery) only for pinned clients.
func (a *clientAuth) filter(raw []byte, m *lazyEventMeta) []byte {
	msgType := m.typ()
	if msgType == "" || !a.mayReceive(msgType) {
		return nil
	}
	if msgType == EventConfig && !config.APITokenScopeAllows(a.principal.Scopes, "config") {
		if raw = redactConfigLocalPaths(raw); raw == nil {
			return nil
		}
	}
	if a.view.sessionID == "" {
		return raw
	}
	return a.view.project(raw, m.meta())
}

func hasScope(principal Principal, scope string) bool {
	for _, granted := range principal.Scopes {
		if granted == scope || granted == "admin" {
			return true
		}
	}
	return false
}

func authorizeWSCommand(auth clientAuth, command string) error {
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
