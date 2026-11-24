package finqueue

import (
	"context"
	"database/sql"
	"errors"
)

// EnqueuePort gives the component typed access to service state and callbacks.
type EnqueuePort[T, A any] struct {
	Prepare       func(context.Context, string, []byte) (T, error)
	Existing      func(context.Context, string, string, string, T) (bool, error)
	Admit         func(context.Context, string) (func() error, A, error)
	AncestryHash  func(T, A) (string, error)
	Begin         func(context.Context) (*sql.Tx, error)
	Validate      func(context.Context, *sql.Tx, string, string, T, string, A) error
	CheckOutbox   func(context.Context, *sql.Tx, string) (bool, error)
	CheckContinue func(context.Context, *sql.Tx, string) (bool, error)
	CheckQueue    func(context.Context, *sql.Tx, string) (string, string, bool, error)
	Enqueue       func(context.Context, *sql.Tx, string, string, []byte) error
	Persist       func(context.Context, *sql.Tx, string, string, T, string) error
	Changed       error
}

// Enqueue preserves admission, ancestry hash, writer transaction, validation,
// duplicate checks, enqueue, row verification, revalidation, persistence, commit.
func Enqueue[T, A any](ctx context.Context, rowID, owner string, raw []byte, port EnqueuePort[T, A]) (retErr error) {
	payload, err := port.Prepare(ctx, owner, raw)
	if err != nil {
		return err
	}
	if handled, err := port.Existing(ctx, rowID, owner, string(raw), payload); handled || err != nil {
		return err
	}
	release, admission, err := port.Admit(ctx, owner)
	if release != nil {
		defer func() { retErr = joinEnqueueRelease(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	ancestryHash, err := port.AncestryHash(payload, admission)
	if err != nil {
		return err
	}
	tx, err := port.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := port.Validate(ctx, tx, owner, rowID, payload, ancestryHash, admission); err != nil {
		return err
	}
	if exists, err := port.CheckOutbox(ctx, tx, rowID); err != nil {
		return err
	} else if exists {
		return port.Changed
	}
	if exists, err := port.CheckContinue(ctx, tx, rowID); err != nil {
		return err
	} else if exists {
		return port.Changed
	}
	if _, _, exists, err := port.CheckQueue(ctx, tx, rowID); err != nil {
		return err
	} else if exists {
		return port.Changed
	}
	if err := port.Enqueue(ctx, tx, rowID, owner, raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	actualOwner, actualRaw, exists, err := port.CheckQueue(ctx, tx, rowID)
	if err != nil {
		return err
	}
	if !exists || actualOwner != owner || actualRaw != string(raw) {
		return port.Changed
	}
	if err := port.Validate(ctx, tx, owner, rowID, payload, ancestryHash, admission); err != nil {
		return err
	}
	if err := port.Persist(ctx, tx, rowID, owner, payload, ancestryHash); err != nil {
		return err
	}
	return tx.Commit()
}

func joinEnqueueRelease(primary, release error) error {
	if release == nil {
		return primary
	}
	if primary == nil {
		return release
	}
	return errors.Join(primary, release)
}
