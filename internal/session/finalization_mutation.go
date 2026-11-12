package session

import (
	"context"
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// admitFinalizationMutation uses only capabilities carried by the producer.
// A missing capability is not reconstructed from persisted IDs; the writer
// still rechecks legacy absence inside its transaction.
func admitFinalizationMutation(ctx context.Context, writer *sql.DB, owners ...string) (func() error, *batchFinalizationAdmission, error) {
	authorities, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	if len(authorities) == 0 {
		return nil, nil, nil
	}
	if authorities[0] == nil {
		return nil, nil, ErrAgentFinalizationChanged
	}
	identity := BatchCommitIdentity{
		finalizationService:     authorities[0].service,
		finalizationAuthorities: authorities,
	}
	return admitBatchCommitOwners(NewBatchStore(writer), ctx, identity, jobbatch.Transition{}, owners)
}
