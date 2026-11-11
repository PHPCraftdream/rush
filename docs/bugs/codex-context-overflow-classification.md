# Codex context-overflow classification incident

## Historical incident evidence

Persisted failure evidence included the exact structured prefix `context_length_exceeded`. The launch was PID 19068, version `0.2.0-alpha.19` (`2a661a17-dirty`, built `2026-11-10_17:30:28`). Last successful usage was 921,485 input tokens (141 uncached, 921,344 cached) and 117 output tokens. No summaries were present across 1,577 messages. Four finish errors were recorded at 02:15:23Z, 02:34:55Z, 03:06:22Z, and 03:06:33Z. The run envelope recorded `exit_reason=error`, duration 5,527,204 ms, and empty `final_text`. The incident report claimed executor `bri2pcflb` exited 1; that outcome is not independently established.

## What the historical source establishes

In the pre-recovery source path, the Codex SSE parser mapped the structured error code to status 0 without setting `ContextTooLargeErr`; the classification change preserves that flag for exact context-overflow codes. Drain overflow guidance checked the flag, HTTP 413, or selected HTTP 400 message text. The recorded source flow returned on this failure before compaction: it did not automatically summarize and replay the request. The installed build's parser state and emitted guidance were not independently captured.

The source metadata records a 922k maximum input against a 1.03m gate, documenting an output-reserve mismatch and a plausible prevention issue. It does not establish the installed build's effective gate: the installed dirty configuration and capacity are unknown. The exact installed state, complete request, and cause of the provider rejection remain unverified.

## Current runtime recovery behavior

The runtime implementation now includes a one-shot automatic recovery path; this is no longer classification-only. Recovery is eligible only for an exact structured `context_length_exceeded` error certified before any output or usage (HTTP 400 or SSE `response.failed` or strictly guarded nested/flat `type=error` envelopes). Narrowly validated nested response lifecycle configuration metadata does not count as output; accepted or opaque output and any usage preclude recovery. Partial, opaque, or uncertified output, transport failures, and opaque or uncertified errors are not replayed.

For an eligible failure, the runtime attempts a silent-prefix summary with the ordinary worker while retaining the same mailbox admission and OS session lock. Compaction preserves the context pin, original user message, and accepted tool-call tail. It then makes one continuation using the same original model and persisted history. An overflow on that continuation is terminal; there is no second recovery cycle.

Proactive capacity admission subtracts the effective output reserve rather than using a hard-coded model limit, and counts cache occupancy. If capacity is unknown, or summary fails, overflows, is cancelled, exceeds its budget, or the worker is missing/unavailable or resolves to an unsupported CLI provider, recovery stops and surfaces the failure. Original history is not deleted unless normal summary publication succeeds.

## Verification status and limits

Tests have not been run and CI is pending; this document does not claim runtime behavior is verified or CI passes. The original executor outcome, installed dirty configuration, and effective capacity remain unknown. No prompt, tool content, or credentials are included here.
