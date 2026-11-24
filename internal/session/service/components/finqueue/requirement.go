package finqueue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	finterm "github.com/PHPCraftdream/rush/internal/session/batch/finterm"
)

// PayloadHash returns the SHA-256 hash of the JSON encoding of data.
func PayloadHash[T any](data T) (string, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return finalizationHash(encoded), nil
}

func HashEncoded(encoded []byte) string {
	return finalizationHash(encoded)
}

func finalizationHash(encoded []byte) string {
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

// Requirement is a declaration, never an authority capability. Only a trusted
// enqueue can persist its matching continuation provenance.
type Requirement struct {
	Version     int                                 `json:"version"`
	Authorities []finterm.AgentFinalizationIdentity `json:"authorities"`
}

// CanonicalRequirement returns the stable JSON form of a validated declaration.
func CanonicalRequirement(requirement *Requirement) (string, error) {
	if requirement == nil || requirement.Version != 1 || len(requirement.Authorities) == 0 || len(requirement.Authorities) > 2048 {
		return "", finterm.ErrAgentFinalizationChanged
	}
	copy := *requirement
	copy.Authorities = slices.Clone(requirement.Authorities)
	slices.SortFunc(copy.Authorities, func(a, b finterm.AgentFinalizationIdentity) int {
		return strings.Compare(a.SessionID, b.SessionID)
	})
	for i, identity := range copy.Authorities {
		if !identity.Valid() || i > 0 && copy.Authorities[i-1].SessionID == identity.SessionID {
			return "", finterm.ErrAgentFinalizationChanged
		}
	}
	encoded, err := json.Marshal(copy)
	return string(encoded), err
}
