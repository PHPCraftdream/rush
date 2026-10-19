package agent

import (
	"errors"
	"sync"

	"charm.land/fantasy"
)

type batchCredentialKey struct {
	registryID string
	sessionID  string
	messageID  string
	callID     string
	tool       string
}

type batchCredentialEntry struct {
	root     string
	creds    *CredentialSet
	consumed bool
}

type batchCredentialEscrow struct {
	mu      sync.Mutex
	entries map[batchCredentialKey]*batchCredentialEntry
}

type batchCredentialLeaf struct {
	fantasy.AgentTool
	escrow *batchCredentialEscrow
}

func (l *batchCredentialLeaf) credentialEscrow() *batchCredentialEscrow { return l.escrow }

func newBatchCredentialEscrow() *batchCredentialEscrow {
	return &batchCredentialEscrow{entries: make(map[batchCredentialKey]*batchCredentialEntry)}
}

var errBatchCredentialEscrowMissing = errors.New("batch credential escrow entry missing or already consumed")

func cloneCredentialSet(creds *CredentialSet) *CredentialSet {
	if creds == nil {
		return nil
	}
	clone := creds.Clone()
	return &clone
}

func (e *batchCredentialEscrow) register(key batchCredentialKey, root string, creds *CredentialSet) error {
	_, err := e.registerBatch([]batchCredentialRegistration{{key: key, root: root, creds: creds}})
	return err
}

type batchCredentialRegistration struct {
	key   batchCredentialKey
	root  string
	creds *CredentialSet
}

func (e *batchCredentialEscrow) registerBatch(registrations []batchCredentialRegistration) (func(), error) {
	if e == nil {
		return nil, errBatchCredentialEscrowMissing
	}
	entries := make(map[batchCredentialKey]*batchCredentialEntry, len(registrations))
	for _, registration := range registrations {
		key := registration.key
		if key.registryID == "" || key.sessionID == "" || key.callID == "" || key.tool == "" || registration.root == "" {
			return nil, errBatchCredentialEscrowMissing
		}
		if _, duplicate := entries[key]; duplicate {
			return nil, errors.New("batch credential escrow duplicate launch")
		}
		entries[key] = &batchCredentialEntry{root: registration.root, creds: cloneCredentialSet(registration.creds)}
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

func (e *batchCredentialEscrow) consume(key batchCredentialKey, nodeID string) (*CredentialSet, func(), error) {
	if e == nil {
		return nil, nil, errBatchCredentialEscrowMissing
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.entries[key]
	if !ok || entry.consumed || (nodeID != entry.root && (len(nodeID) <= len(entry.root) || nodeID[:len(entry.root)+1] != entry.root+".")) {
		return nil, nil, errBatchCredentialEscrowMissing
	}
	entry.consumed = true
	creds := cloneCredentialSet(entry.creds)
	entry.creds = nil
	return creds, func() { e.releaseEntry(key, entry) }, nil
}

func (e *batchCredentialEscrow) releaseEntries(entries map[batchCredentialKey]*batchCredentialEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, entry := range entries {
		if e.entries[key] == entry {
			entry.creds = nil
			delete(e.entries, key)
		}
	}
}

func (e *batchCredentialEscrow) releaseEntry(key batchCredentialKey, entry *batchCredentialEntry) {
	e.mu.Lock()
	if e.entries[key] == entry {
		entry.creds = nil
		delete(e.entries, key)
	}
	e.mu.Unlock()
}

func (e *batchCredentialEscrow) releaseRoot(root string) {
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

func batchCredentialKeyFor(p batchLeafPayload) batchCredentialKey {
	messageID := ""
	if p.Policy != nil {
		messageID = p.Policy.MessageID
	}
	return batchCredentialKey{registryID: p.RegistryID, sessionID: p.SessionID, messageID: messageID, callID: p.ToolCallID, tool: p.Tool}
}
