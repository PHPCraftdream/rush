// Pure metadata classifier for async job tool results, split out of
// ActionRow.tsx so it can be unit-tested without a React renderer.
//
// A14: an inline result (the tool call's own answer to a fast command,
// `async:false` + `inline:true`) is NOT pending -- it already carries the
// job's outcome. Only a real "started" response (`async:true` + `job_id`)
// or a background shell (`background:true` + `shell_id`) stays pending.

export function isPendingJob(metadata?: string): boolean {
  if (!metadata) return false;
  try {
    const parsed = JSON.parse(metadata) as {
      async?: boolean;
      inline?: boolean;
      job_id?: string;
      background?: boolean;
      shell_id?: string;
    };
    if (parsed.async === false) return false;
    return (parsed.async === true && !!parsed.job_id) || (parsed.background === true && !!parsed.shell_id);
  } catch {
    return false;
  }
}
