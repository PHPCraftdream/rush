package sesslocks

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// BatchKillScopeState is the batch-scope verdict the kill command's injected
// batch-note reader returns (the root batchKillLiveNote provider).
type BatchKillScopeState struct {
	Note        string
	Unconfirmed bool
}

// BatchKillNoteReader is the injected provider type for batchKillLiveNote.
type BatchKillNoteReader func(context.Context, string, string) BatchKillScopeState

var errBatchKillScopeUnconfirmed = errors.New("batch scope termination is unconfirmed")

func readBoundedBatchKillNote(ctx context.Context, dataDir, id string, read BatchKillNoteReader) BatchKillScopeState {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return read(ctx, dataDir, id)
}

// Свободный turn lock не доказывает остановку независимого batch scope.
func batchKillUnprovenError(state BatchKillScopeState) error {
	if !state.Unconfirmed {
		return nil
	}
	return fmt.Errorf("%w: no turn-lock holder was killed; queued work is preserved; stop batch work explicitly: %s", errBatchKillScopeUnconfirmed, state.Note)
}
