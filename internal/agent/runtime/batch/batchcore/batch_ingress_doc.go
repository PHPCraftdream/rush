// Package batchcore owns prepared-call singleflight, durable attach routing,
// and delivery of already-started batch roots. The agent adapter validates and
// prepares the call before invoking the core; on an attach miss the core calls
// the submit port, which must admit the spec before touching the engine.
//
// Live attach is fenced by the invoking owner, call identity and input hash,
// submitting host and runner incarnation, and optional lease validation.
// Delivery never submits: it either returns the real executor envelope or
// hands completion to the durable notice path through a started election.
// Cancellation is a stop request, not evidence of physical settlement.
package batchcore

// batchIngressDoc marks the ingress contract: the caller pins the registry
// identity and validates the invocation before the core routes attach or submit.
// The runner uses the caller-supplied root ID; durable attach fences the
// accepted spec by input hash rather than treating a call ID alone as a match.
type batchIngressDoc struct{}
