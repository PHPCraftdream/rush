package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type batchKillScopeState struct {
	Note        string
	Unconfirmed bool
}

type batchKillNoteReader func(context.Context, string, string) batchKillScopeState

var errBatchKillScopeUnconfirmed = errors.New("batch scope termination is unconfirmed")

func readBoundedBatchKillNote(ctx context.Context, dataDir, id string, read batchKillNoteReader) batchKillScopeState {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return read(ctx, dataDir, id)
}

// Свободный turn lock не доказывает остановку независимого batch scope.
func batchKillUnprovenError(state batchKillScopeState) error {
	if !state.Unconfirmed {
		return nil
	}
	return fmt.Errorf("%w: no turn-lock holder was killed; queued work is preserved; stop batch work explicitly: %s", errBatchKillScopeUnconfirmed, state.Note)
}
