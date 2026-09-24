Launch a new agent that has access to the following tools: glob, grep, ls, view. When you are searching for a keyword or file and are not confident that you will find the right match on the first try, use the agent tool to perform the search for you.

In web and CLI sessions the call returns a job ID immediately. Continue independent work; Rush adds the child's final answer or error as a new message in this session and resumes you. Do not call this tool again just to check whether the child finished.

When running as a smart orchestrator with a worker model configured, the sub-agent may also get hands-on tools (edit, multiedit, write, bash, todos, download, fetch, ask_question) to actually perform delegated work, not just search.

If the sub-agent calls its own question tool, its completion message (or the direct tool result outside web/CLI) says `SUB-AGENT QUESTION (session <id>): <question>`. This is a pause, not a crash. Decide the answer, or ask the operator when necessary, then call this tool with `resume_session_id` set to that child session and the answer as `prompt`. The child's context is preserved; unrelated session IDs are rejected.
