package server

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
)

// EffectiveListenerPins resolves listener declarations against immutable token
// bindings. Any configured listener pin makes the listener pinned-token-only.
func EffectiveListenerPins(declared map[string]string, tokens []config.APIToken, logger *slog.Logger) (map[string]string, error) {
	byID := make(map[string]config.APIToken, len(tokens))
	for _, token := range tokens {
		byID[token.ID] = token
	}
	effective := make(map[string]string)
	for _, token := range tokens {
		binding := token.Binding.Normalized()
		if binding.Kind != "none" && containsScope(token.Scopes, "admin") {
			return nil, fmt.Errorf("token %s combines admin scope with a binding", token.ID)
		}
		if binding.Kind == "session" && binding.Pin != nil {
			effective[token.ID] = binding.Pin.Session
			if _, err := config.CanonicalAPIWorkspace(binding.Pin.Workspace); err != nil {
				return nil, fmt.Errorf("token %s has invalid pinned workspace: %w", token.ID, err)
			}
		} else if binding.Kind != "none" && binding.Kind != "batch" {
			return nil, fmt.Errorf("token %s binding kind %q is invalid", token.ID, binding.Kind)
		}
	}
	keys := make([]string, 0, len(declared))
	for id := range declared {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		sessionID := strings.TrimSpace(declared[id])
		if id == "" || sessionID == "" {
			return nil, errors.New("listener pins require non-empty token and session ids")
		}
		token, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("listener pin references unknown token %s", id)
		}
		if containsScope(token.Scopes, "admin") {
			return nil, fmt.Errorf("token %s combines admin scope with a listener pin", id)
		}
		if existing := effective[id]; existing != "" && existing != sessionID {
			return nil, fmt.Errorf("listener pin for token %s conflicts with its binding", id)
		}
		if token.Binding.Normalized().Kind == "batch" {
			return nil, fmt.Errorf("listener session pin cannot override batch binding for token %s", id)
		}
		effective[id] = sessionID
	}
	for _, token := range tokens {
		if binding := token.Binding; binding.Kind == "session" && binding.Pin != nil && binding.Pin.Create {
			if _, explicitlyDeclared := declared[token.ID]; explicitlyDeclared && declared[token.ID] != binding.Pin.Session {
				return nil, fmt.Errorf("listener pin for token %s conflicts with its binding", token.ID)
			}
		}
	}
	return effective, nil
}

func containsScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

// ValidatePinnedPermissionModes prevents permission-policy escalation through
// multiple credentials sharing one pinned session.
func ValidatePinnedPermissionModes(tokens []config.APIToken, pins map[string]string) error {
	modes := make(map[string]string)
	for _, token := range tokens {
		sessionID := pins[token.ID]
		if sessionID == "" {
			continue
		}
		if previous, ok := modes[sessionID]; ok && previous != token.Permission {
			return fmt.Errorf("tokens pinned to session %s have different permission modes", sessionID)
		}
		modes[sessionID] = token.Permission
	}
	return nil
}

func ValidatePinnedPermissionModesAcrossPolicies(tokens []config.APIToken, policies []ListenerPolicy) error {
	permissions := make(map[string]string)
	for _, policy := range policies {
		for tokenID, sessionID := range policy.Pins {
			for _, token := range tokens {
				if token.ID != tokenID {
					continue
				}
				if previous, ok := permissions[sessionID]; ok && previous != token.Permission {
					return fmt.Errorf("tokens pinned to session %s have different permission modes", sessionID)
				}
				permissions[sessionID] = token.Permission
				break
			}
		}
	}
	return nil
}

// PinnedTokenIDs returns a stable allow-list for authentication wiring.
func PinnedTokenIDs(pins map[string]string) []string {
	ids := make([]string, 0, len(pins))
	for id := range pins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ListenerDeclaresPins distinguishes an absent map from an explicitly populated map.
func ListenerDeclaresPins(pins map[string]string) bool { return len(pins) != 0 }
