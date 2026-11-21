package batchadapter

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// StoreAttach adapts session.BatchStore onto the R11B attach seam.
type StoreAttach struct{ Store *session.BatchStore }

var _ batchcore.AttachStore = StoreAttach{}

// ProbeLive is the read-only fence probe; reservations happen in the commit
// hook at Submit, never here.
func (a StoreAttach) ProbeLive(ctx context.Context, p batchcore.AttachProbe) (jobbatch.ID, bool, error) {
	root, ok, err := a.Store.BatchProbeLive(ctx, session.BatchLiveProbe{
		Owner: p.Owner, Host: p.Host, RunnerID: p.RunnerID,
		ToolCallID: p.ToolCallID, InputHash: p.InputHash,
		SubmittedMessageID: p.SubmittedMessageID, SubmittedRegistryID: p.SubmittedRegistryID,
	})
	return jobbatch.ID(root), ok, err
}
