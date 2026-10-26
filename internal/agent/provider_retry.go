package agent

// ProviderMaxRetries pins the fantasy retry COUNT at the pre-v0.45 value. The
// v0.45 bump raised the default from 2 retries to 3, which tripled the total
// wait of an exhausted retry flow (5+10+20 s vs 2+4 s) and pushed internal/app
// close to its CI timeout. Every fantasy agent call/stream call below sets
// MaxRetries from this var so attempt counts stay as before the bump. The
// library's initial delay (5 s vs the old 2 s) cannot be pinned through the
// agent API (fantasy v0.45.2 builds DefaultRetryOptions() per call and only
// MaxRetries is overridable) and is an accepted residual; a Retry-After
// header of up to 60 s still wins over the exponential delay, so
// server-directed rate-limit waits are unchanged.
var ProviderMaxRetries = 2
