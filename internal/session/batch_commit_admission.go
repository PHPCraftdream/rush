package session

import (
	"context"
	"errors"
	"slices"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// admitBatchCommitOwners resolves one ordered, distinct ticket set before SQL.
func admitBatchCommitOwners(ctx context.Context, id BatchCommitIdentity, transition jobbatch.Transition, owners []string) (func(), error) {
	if id.AdmitOwners == nil || len(owners) == 0 {
		return nil, nil
	}
	owners = slices.Clone(owners)
	slices.Sort(owners)
	owners = slices.Compact(owners)
	if owners[0] == "" {
		return nil, errors.New("batch commit admission: missing owner")
	}
	release, err := id.AdmitOwners(ctx, transition, owners)
	if err == nil && release == nil {
		return nil, errors.New("batch commit admission: missing release")
	}
	return release, err
}
