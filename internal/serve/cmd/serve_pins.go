package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
)

func parseServePinSpecs(specs []string) (map[string]string, error) {
	pins := make(map[string]string, len(specs))
	for _, spec := range specs {
		id, sessionID, ok := strings.Cut(spec, "=")
		id, sessionID = strings.TrimSpace(id), strings.TrimSpace(sessionID)
		if !ok || id == "" || sessionID == "" {
			return nil, errors.New("--pin-session requires TOKEN_ID=SESSION_ID")
		}
		if _, exists := pins[id]; exists {
			return nil, fmt.Errorf("duplicate --pin-session for token %s", id)
		}
		pins[id] = sessionID
	}
	return pins, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func canonicalServeWorkspace(cwd string) (string, error) {
	return config.CanonicalAPIWorkspace(cwd)
}
