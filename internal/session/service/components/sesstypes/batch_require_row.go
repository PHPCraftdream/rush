package sesstypes

import "errors"

// ErrBatchProjectionConflict reports a projection whose row-count guard did
// not match exactly one row (staged here with BatchRequireRow until the
// sessionbatch move; see MV1 report, deviation (c), resolving lane MV3).
var ErrBatchProjectionConflict = errors.New("batch store: projection identity or sequence conflict")

func BatchRequireRow(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrBatchProjectionConflict
	}
	return nil
}
