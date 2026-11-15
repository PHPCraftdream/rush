package cmd

import (
	"context"
	"errors"
)

type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

func apiExitCode(err error) (int, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return 5, true
	}
	var coded *exitCodeError
	if !errors.As(err, &coded) {
		return 0, false
	}
	switch coded.code {
	case 0, 1, 2, 3, 4, 5, 6, 130:
		return coded.code, true
	default:
		return 0, false
	}
}
