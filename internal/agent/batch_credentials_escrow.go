package agent

import (
	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter"
)

type batchCredentialKey struct {
	registryID string
	sessionID  string
	messageID  string
	callID     string
	tool       string
}

type batchCredentialLeaf struct {
	fantasy.AgentTool
	escrow *batchCredentialEscrow
}

func (l *batchCredentialLeaf) credentialEscrow() *batchCredentialEscrow { return l.escrow }

// PolicyLeafTool is the production role-introspection seam: HEAD role
// introspection unwraps this wrapper to reach the leaf tool underneath.
func (l *batchCredentialLeaf) PolicyLeafTool() fantasy.AgentTool { return l.AgentTool }

// UnwrapCapturedCredentialLeaf is the narrow credential-escrow capability the
// moved captured walk (toolwrap.CapturedToolFrom) uses: BatchLeafRegistryStore
// .Register wraps every agent leaf, so the executor-side captured lookup must
// see through this exact wrapper.
func (l *batchCredentialLeaf) UnwrapCapturedCredentialLeaf() fantasy.AgentTool {
	return l.AgentTool
}

type batchCredentialEscrow struct{ *batchadapter.CredentialEscrow }

func newBatchCredentialEscrow() *batchCredentialEscrow {
	return &batchCredentialEscrow{batchadapter.NewCredentialEscrow()}
}

var errBatchCredentialEscrowMissing = batchadapter.ErrCredentialEscrowMissing

func (k batchCredentialKey) adapterKey() batchadapter.CredentialKey {
	return batchadapter.CredentialKey{RegistryID: k.registryID, SessionID: k.sessionID, MessageID: k.messageID, CallID: k.callID, Tool: k.tool}
}

func (e *batchCredentialEscrow) adapterEscrow() *batchadapter.CredentialEscrow {
	if e == nil {
		return nil
	}
	return e.CredentialEscrow
}

func (e *batchCredentialEscrow) consume(key batchCredentialKey, nodeID string) (*CredentialSet, func(), error) {
	return e.adapterEscrow().Consume(key.adapterKey(), nodeID)
}

func (e *batchCredentialEscrow) releaseRoot(root string) { e.adapterEscrow().ReleaseRoot(root) }

type batchCredentialRegistration struct {
	key   batchCredentialKey
	root  string
	creds *CredentialSet
}

func batchCredentialKeyFor(p batchLeafPayload) batchCredentialKey {
	messageID := ""
	if p.Policy != nil {
		messageID = p.Policy.MessageID
	}
	key := batchadapter.CredentialKeyFor(p.RegistryID, p.SessionID, messageID, p.ToolCallID, p.Tool)
	return batchCredentialKey{registryID: key.RegistryID, sessionID: key.SessionID, messageID: key.MessageID, callID: key.CallID, tool: key.Tool}
}
