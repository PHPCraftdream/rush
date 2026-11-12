/**
 * Patterns of the completion notices the server writes into the transcript.
 *
 * `finished` matches the finished/failed notice. `stopped` matches the
 * non-finished terminal causes, each mapped to its own status so the tool-call
 * block can show "timed out" / "stopped by user" / "cancelled" distinctly (stage
 * 5b); same lead-in by construction (both are FormatAsyncCompletion outputs).
 * `legacy` matches the pre-batch background shell notice.
 */
export const ASYNC_NOTICE_PATTERNS = {
  finished: /^Async job (\S+) \([^)]*\) (finished|failed)\.\n\n([\s\S]*)$/,
  stopped: new RegExp(
    [
      /^Async job (\S+) \([^)]*\) /.source,
      /(timed out after \d+s and was stopped|was stopped \(batch_stop\)|was cancelled \(session stopped\))/.source,
      /\. Partial output( before the stop)?:\n\n([\s\S]*)$/.source,
    ].join(""),
  ),
  legacy: /^Background job (\S+) \([^\n]*\) finished: exit (-?\d+), ran [^\n]+\./,
};
