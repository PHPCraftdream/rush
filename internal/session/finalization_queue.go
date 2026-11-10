package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

// AgentFinalizationRequirement is a declaration, never an authority capability.
// Only a trusted enqueue can persist its matching continuation provenance.
type AgentFinalizationRequirement struct {
	Version     int                         `json:"version"`
	Authorities []AgentFinalizationIdentity `json:"authorities"`
}

// Requirement returns a detached declaration suitable for durable call data.
func (snapshot AgentFinalizationContext) Requirement() *AgentFinalizationRequirement {
	if len(snapshot.authorities) == 0 {
		return nil
	}
	requirement := &AgentFinalizationRequirement{Version: 1}
	for _, authority := range snapshot.authorities {
		requirement.Authorities = append(requirement.Authorities, authority.identity)
	}
	slices.SortFunc(requirement.Authorities, func(a, b AgentFinalizationIdentity) int {
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return requirement
}

func canonicalFinalizationRequirement(requirement *AgentFinalizationRequirement) (string, error) {
	if requirement == nil || requirement.Version != 1 || len(requirement.Authorities) == 0 || len(requirement.Authorities) > 2048 {
		return "", ErrAgentFinalizationChanged
	}
	copy := *requirement
	copy.Authorities = slices.Clone(requirement.Authorities)
	slices.SortFunc(copy.Authorities, func(a, b AgentFinalizationIdentity) int {
		return strings.Compare(a.SessionID, b.SessionID)
	})
	for i, identity := range copy.Authorities {
		if !identity.valid() || i > 0 && copy.Authorities[i-1].SessionID == identity.SessionID {
			return "", ErrAgentFinalizationChanged
		}
	}
	encoded, err := json.Marshal(copy)
	return string(encoded), err
}

func finalizationPayloadHash(data SessionAgentCallData) (string, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return finalizationHash(encoded), nil
}

func finalizationHash(encoded []byte) string {
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func finalizationAncestryHash(ancestry finalizationAncestry) string {
	type edge struct {
		Node, Owner, Child string
		Token              int64
	}
	snapshot := struct {
		Owners []string
		Edges  []edge
	}{Owners: ancestry.owners}
	for _, item := range ancestry.edges {
		snapshot.Edges = append(snapshot.Edges, edge{Node: item.node, Owner: item.owner, Child: item.child, Token: item.token})
	}
	encoded, _ := json.Marshal(snapshot) // Only strings and integers.
	return finalizationHash(encoded)
}

func trustedFinalizationDeclaration(ctx context.Context, writerService *service, data SessionAgentCallData) (string, error) {
	declaration, err := canonicalFinalizationRequirement(data.FinalizationRequirement)
	if err != nil {
		return "", err
	}
	snapshot := CaptureAgentFinalizationContext(ctx)
	for _, authority := range snapshot.authorities {
		if authority == nil || authority.service == nil || authority.service.db != writerService.db {
			return "", ErrAgentFinalizationChanged
		}
		if authority.identity.SessionID == data.SessionID && authority.identity.LogicalCallID != data.LogicalCallID {
			return "", ErrAgentFinalizationChanged
		}
	}
	trusted, err := canonicalFinalizationRequirement(snapshot.Requirement())
	if err != nil || declaration != trusted {
		return "", ErrAgentFinalizationChanged
	}
	return declaration, nil
}
