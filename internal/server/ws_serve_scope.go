package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	if c == nil || c.auth == nil || msgType != EventMCPState {
		return true
	}
	return c.auth.policy.AllowAdmin && hasScope(c.auth.principal, "admin")
}

// filterMetadata reports whether this client may receive the event. The cheap
// type check runs for every client; the payload is parsed (once per delivery)
// only for pinned clients.
func (c *Client) filterMetadata(m *lazyEventMeta) bool {
	if !c.mayReceive(m.typ()) {
		return false
	}
	if c == nil || c.auth == nil || c.auth.view.sessionID == "" {
		return true
	}
	return c.auth.view.allows(m.meta())
}

// replyBypassesView lets the correlated error/ok reply to a command through a
// pinned view: the command already passed the pre-handler session resolution,
// so it targeted the pinned session, and these envelopes carry no event
// payload of another session. Uncorrelated or data-bearing replies still go
// through the view filter.
func (a *clientAuth) replyBypassesView(id, msgType string) bool {
	return a.view.sessionID != "" && id != "" && (msgType == EventError || msgType == EventResponse)
}

func (a *clientAuth) filter(raw []byte, m *lazyEventMeta) []byte {
	msgType := m.typ()
	if msgType == "" || (msgType == EventMCPState && (!a.policy.AllowAdmin || !hasScope(a.principal, "admin"))) {
		return nil
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
