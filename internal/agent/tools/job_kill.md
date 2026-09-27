Terminate a background shell process.

<usage>
- Accepts job_id (preferred -- the id returned when the command started, e.g. "Async bash job <id> started") or shell_id (a raw background shell id). Provide exactly one.
- Cancels the running process and cleans up resources
</usage>

<features>
- Stop long-running background processes
- Clean up completed background shells
- Immediately terminates the process
</features>

<tips>
- Use this when you need to stop a background process
- The process is terminated immediately (similar to SIGTERM)
- After killing, the shell ID becomes invalid
</tips>
