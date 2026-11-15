// Package server provides authentication and abuse protection for external API
// requests. Chain audits every response, applies source-IP back-off before
// authentication, verifies bearer credentials, enforces per-token and per-IP
// rate/concurrency limits, and caps request bodies. Authentication failures use
// one indistinguishable 401 response; token-store failures use a fixed 503.
// Back-off retains failure timestamps from the last 60 seconds, an offence
// counter, blockedUntil, and lastActivity per source IP. The tenth failure in a
// rolling 60-second window starts a one-second block (offence 1). Blocked is a
// pure read: requests rejected during a block do not count or extend it. Each
// further authentication failure during escalation increments offences and
// starts the corresponding exponential delay from that failure, capped at five
// minutes. An offence entry is evicted only after its block ends and it remains
// quiet for one additional current delay; entries with no offence expire after
// 60 seconds without activity. Retry-After is the remaining whole seconds,
// rounded up and never below one. Successful authentication never changes
// back-off state. Store errors are not client failures and do not increment the
// counter. Back-off is keyed by the remote peer address, not forwarding headers: clients sharing a NAT can collectively exhaust the allowance, so
// operators should account for that trade-off when publishing a listener. If the handler panics, Chain emits the audit line with status 500,
// releases its concurrency slot during unwinding, and re-panics the original
// panic. A response whose headers or body have already started cannot be
// rewritten into a 413 or 503 response. An unknown-length body remains streaming
// through MaxBytesReader; overflow produces 413 only if the handler has not
// started the response. Zero limits select defaults of 20 requests per second,
// burst 40, eight concurrent requests per token, and a 1 MiB body.
package server
