# Codex context-overflow classification incident

## Observed incident evidence

Persisted failure evidence included the exact structured prefix `context_length_exceeded`. The launch was PID 19068, version `0.2.0-alpha.19` (`2a661a17-dirty`, built `2026-11-10_17:30:28`). Last successful usage was 921,485 input tokens (141 uncached, 921,344 cached) and 117 output tokens. No summaries were present across 1,577 messages. Four finish errors were recorded at 02:15:23Z, 02:34:55Z, 03:06:22Z, and 03:06:33Z. The run envelope recorded `exit_reason=error`, duration 5,527,204 ms, and empty `final_text`. The incident report claimed executor `bri2pcflb` exited 1; that outcome is not independently established.

## Source-supported mechanism

The pre-change Codex SSE parser mapped the structured error code to status 0 without setting `ContextTooLargeErr`; Drain overflow guidance checks that flag, HTTP 413, or selected HTTP 400 message text. Thus the observed structured code alone could miss the specialized guidance on that source path; the installed build's actual parser state and emitted guidance were not independently captured.

This change classifies only an exact structured `code` or `type` equal to `context_length_exceeded` as context-too-large. It changes remedy guidance only; status, message/body, terminal treatment, and request behavior remain unchanged. No retry, recovery, truncation, compaction, or model switch was added.

The source's 922k maximum input versus a 1.03m gate is a plausible prevention issue, not proof of the installed build's effective gate or the original overflow cause. The exact installed state, effective limits, complete request, and why the original prompt exceeded the provider's accepted context remain unverified. This classification fix does not establish that the overflow was prevented or that the underlying cause was fixed.

No prompt, tool content, or credentials are included here.
