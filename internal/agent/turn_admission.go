package agent

import "context"

// Admission-time session setup (#1101): a caller that prepares a session for
// its run (the one-shot cancel flag, the operator-visible budget, ended_reason)
// attaches those writes here instead of running them before admission, so a
// call refused by the mailbox or the inter-process session lock never touches
// the session's state. runOwned invokes the callback once the mailbox and the
// lock are ours, before the first turn; Drains and loop-owned turns simply
// carry no callback.
type admissionSetupKey struct{}

// WithAdmissionSetup returns a context carrying fn; it replaces any callback
// already present (each caller stamps its own writes).
func WithAdmissionSetup(ctx context.Context, fn func(context.Context) error) context.Context {
	return context.WithValue(ctx, admissionSetupKey{}, fn)
}

func admissionSetupFrom(ctx context.Context) func(context.Context) error {
	fn, _ := ctx.Value(admissionSetupKey{}).(func(context.Context) error)
	return fn
}
