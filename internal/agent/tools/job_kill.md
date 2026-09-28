Terminate a running background bash or run_command job.

<usage>
- Accepts job_id (preferred -- the id returned when the command started, e.g. "Async bash job <id> started") or shell_id (a raw background shell id, bash only). Provide exactly one.
- Cancels the running process (bash: the shell; run_command: the whole process tree it spawned, not just the direct process) and cleans up resources.
- Stopping a job this way produces a distinct "stopped" notice, not a generic failure -- the owner can tell it was cancelled on purpose.
- A job_id for a sub-agent delegation (agent/agentic_fetch) is refused: it cannot be stopped this way yet; its result will arrive as a session message when it finishes.
</usage>

<features>
- Stop long-running background bash commands or run_command programs
- Clean up completed background shells
- Immediately terminates the process (bash: the shell; run_command: its whole process tree)
</features>

<tips>
- Use this when you need to stop a background job
- The process is terminated immediately (similar to SIGTERM)
- After killing, the job/shell ID becomes invalid
- Safe to call again on an already-stopped/finished job_id -- it just says "not found", not a disguised second success
</tips>
