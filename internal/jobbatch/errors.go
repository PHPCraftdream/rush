package jobbatch

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidSpec   = errors.New("jobbatch: invalid spec")
	ErrInvalidEvent  = errors.New("jobbatch: invalid, stale or out-of-order event")
	ErrDuplicateRoot = errors.New("jobbatch: duplicate root")
	ErrUnknownNode   = errors.New("jobbatch: unknown node")
	ErrTerminal      = errors.New("jobbatch: node is terminal")
	ErrNotTerminal   = errors.New("jobbatch: node is not terminal")
	ErrNotStarted    = errors.New("jobbatch: execution not started")
	ErrUnsupported   = errors.New("jobbatch: unsupported for this node or execution")
	ErrReleased      = errors.New("jobbatch: root released")
	ErrClosing       = errors.New("jobbatch: runner closing")
	ErrClosed        = errors.New("jobbatch: runner closed")
	ErrCloseTimeout  = errors.New("jobbatch: close timed out before final")
	ErrGap           = errors.New("jobbatch: feed gap")
	ErrExecutorPanic = errors.New("jobbatch: executor panic")
	ErrExecutionDone = errors.New("jobbatch: execution already done")
	ErrDispose       = errors.New("jobbatch: dispose")
	ErrCapacity      = errors.New("jobbatch: capacity refused")
)

// DisposeFailure names one handle whose cleanup did not succeed.
type DisposeFailure struct {
	Node  ID
	Token uint64
	Code  string // dispose_failed, dispose_panic, dispose_timeout or dispose_pending.
}

// DisposeError is the error behind ErrDispose. It describes handle cleanup,
// never a task outcome.
type DisposeError struct {
	Failures []DisposeFailure
}

func (e *DisposeError) Error() string {
	var b strings.Builder
	b.WriteString(ErrDispose.Error())
	for i, f := range e.Failures {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s#%d %s", f.Node, f.Token, f.Code)
	}
	return b.String()
}

func (e *DisposeError) Is(target error) bool { return target == ErrDispose }

// CloseTimeoutError is the error behind ErrCloseTimeout; it names the nodes
// that had not reached a terminal state.
type CloseTimeoutError struct {
	Nodes []ID
}

func (e *CloseTimeoutError) Error() string {
	var b strings.Builder
	b.WriteString(ErrCloseTimeout.Error())
	for i, n := range e.Nodes {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(string(n))
	}
	return b.String()
}

func (e *CloseTimeoutError) Is(target error) bool { return target == ErrCloseTimeout }

// Reserved runtime machine codes. A raw code from an executor that equals one
// of these is replaced by CodeInvalid before it reaches the engine.
const (
	CodeStartFailed          = "start_failed"
	CodeStartNilExecution    = "start_nil_execution"
	CodeStartPanic           = "start_panic"
	CodeExecutorInvalidState = "executor_invalid_state"
	CodeInvalid              = "invalid_code"
	CodeStopFailed           = "stop_failed"
	CodeStopPanic            = "stop_panic"
	CodeStopTimeout          = "stop_timeout"
	CodeDisposeFailed        = "dispose_failed"
	CodeDisposePanic         = "dispose_panic"
	CodeDisposeTimeout       = "dispose_timeout"
	CodeDisposePending       = "dispose_pending"
)

const (
	// MaxCodeBytes is the longest machine code.
	MaxCodeBytes = 64
	// MaxRefBytes is the longest caller reference.
	MaxRefBytes = 128
)

// ValidCode reports whether code is empty or 1-MaxCodeBytes bytes of
// [a-z0-9_.-].
func ValidCode(code string) bool {
	if len(code) > MaxCodeBytes {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// ReservedCode reports whether code is one of the reserved runtime codes.
func ReservedCode(code string) bool {
	switch code {
	case CodeStartFailed, CodeStartNilExecution, CodeStartPanic,
		CodeExecutorInvalidState, CodeInvalid, CodeStopFailed, CodeStopPanic,
		CodeStopTimeout, CodeDisposeFailed, CodeDisposePanic, CodeDisposeTimeout,
		CodeDisposePending:
		return true
	}
	return false
}

// ValidRef reports whether ref is empty or 1-MaxRefBytes bytes of
// [A-Za-z0-9._:-].
func ValidRef(ref string) bool {
	if len(ref) > MaxRefBytes {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}
