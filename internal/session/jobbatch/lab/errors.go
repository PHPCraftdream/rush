package lab

import "errors"

var (
	ErrUnknownAttempt = errors.New("batchlab: unknown attempt")
	ErrAttemptPhase   = errors.New("batchlab: invalid attempt phase")
)
