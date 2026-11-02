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
}

func newClientAuth(principal Principal, policy CommandPolicy) *clientAuth {
	principal.Scopes = append([]string(nil), principal.Scopes...)
	return &clientAuth{principal: principal, policy: policy}
}

func (c *Client) mayReceive(msgType string) bool {
	if c == nil || c.auth == nil || msgType != EventMCPState {
		return true
	}
	return c.auth.policy.AllowAdmin && hasScope(c.auth.principal, "admin")
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
