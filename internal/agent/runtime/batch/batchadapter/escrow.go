package batchadapter

import (
	"errors"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
)

type CredentialKey struct {
	RegistryID string
	SessionID  string
	MessageID  string
	CallID     string
	Tool       string
}

// CredentialKeyFor constructs the escrow identity from trusted launch fields.
// messageID is empty when the launch carries no policy context.
func CredentialKeyFor(registryID, sessionID, messageID, callID, tool string) CredentialKey {
	return CredentialKey{RegistryID: registryID, SessionID: sessionID, MessageID: messageID, CallID: callID, Tool: tool}
}

type credentialEntry struct {
	root     string
	creds    *agentcall.CredentialSet
	consumed bool
}

type CredentialEscrow struct {
	mu      sync.Mutex
	entries map[CredentialKey]*credentialEntry
}

func NewCredentialEscrow() *CredentialEscrow {
	return &CredentialEscrow{entries: make(map[CredentialKey]*credentialEntry)}
}

var ErrCredentialEscrowMissing = errors.New("batch credential escrow entry missing or already consumed")

func cloneCredentialSet(creds *agentcall.CredentialSet) *agentcall.CredentialSet {
	if creds == nil {
		return nil
	}
	clone := creds.Clone()
	return &clone
}

func (e *CredentialEscrow) Register(key CredentialKey, root string, creds *agentcall.CredentialSet) error {
	_, err := e.RegisterBatch([]CredentialRegistration{{Key: key, Root: root, Credentials: creds}})
	return err
}

type CredentialRegistration struct {
	Key         CredentialKey
	Root        string
	Credentials *agentcall.CredentialSet
}

func (e *CredentialEscrow) RegisterBatch(registrations []CredentialRegistration) (func(), error) {
	if e == nil {
		return nil, ErrCredentialEscrowMissing
	}
	entries := make(map[CredentialKey]*credentialEntry, len(registrations))
	for _, registration := range registrations {
		key := registration.Key
		if key.RegistryID == "" || key.SessionID == "" || key.CallID == "" || key.Tool == "" || registration.Root == "" {
			return nil, ErrCredentialEscrowMissing
		}
		if _, duplicate := entries[key]; duplicate {
			return nil, errors.New("batch credential escrow duplicate launch")
		}
		entries[key] = &credentialEntry{root: registration.Root, creds: cloneCredentialSet(registration.Credentials)}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for key := range entries {
		if _, exists := e.entries[key]; exists {
			return nil, errors.New("batch credential escrow duplicate launch")
		}
	}
	for key, entry := range entries {
		e.entries[key] = entry
	}
	return func() { e.releaseEntries(entries) }, nil
}

func (e *CredentialEscrow) Consume(key CredentialKey, nodeID string) (*agentcall.CredentialSet, func(), error) {
	if e == nil {
		return nil, nil, ErrCredentialEscrowMissing
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.entries[key]
	if !ok || entry.consumed || (nodeID != entry.root && (len(nodeID) <= len(entry.root) || nodeID[:len(entry.root)+1] != entry.root+".")) {
		return nil, nil, ErrCredentialEscrowMissing
	}
	entry.consumed = true
	creds := cloneCredentialSet(entry.creds)
	entry.creds = nil
	return creds, func() { e.releaseEntry(key, entry) }, nil
}

func (e *CredentialEscrow) releaseEntries(entries map[CredentialKey]*credentialEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, entry := range entries {
		if e.entries[key] == entry {
			entry.creds = nil
			delete(e.entries, key)
		}
	}
}

func (e *CredentialEscrow) releaseEntry(key CredentialKey, entry *credentialEntry) {
	e.mu.Lock()
	if e.entries[key] == entry {
		entry.creds = nil
		delete(e.entries, key)
	}
	e.mu.Unlock()
}

func (e *CredentialEscrow) ReleaseRoot(root string) {
	if e == nil || root == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, entry := range e.entries {
		if entry.root == root {
			entry.creds = nil
			delete(e.entries, key)
		}
	}
}
