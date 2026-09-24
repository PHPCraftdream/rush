Execute shell commands. In web and CLI sessions every command starts asynchronously and its result arrives as a new session message.

<cross_platform>
Uses mvdan/sh interpreter (Bash-compatible on all platforms including Windows).
Use forward slashes for paths: "ls C:/foo/bar" not "ls C:\foo\bar".
Common shell builtins and core utils available on Windows.
</cross_platform>

<execution_steps>
1. Directory Verification: If creating directories/files, use LS tool to verify parent exists
2. Security Check: Banned commands ({{ .BannedCommands }}) return error - explain to user. Safe read-only commands execute without prompts
3. Command Execution: Execute with proper quoting, capture output
4. Web/CLI execution returns a job ID immediately; keep working while Rush delivers completion
5. Output Processing: Truncate if exceeds {{ .MaxOutputLength }} characters
6. Read the later completion message for output and exit status
</execution_steps>

<usage_notes>
- Command required, working_dir optional (defaults to current directory)
- IMPORTANT: Use Grep/Glob/Agent tools instead of 'find'/'grep'. Use View/LS tools instead of 'cat'/'head'/'tail'/'ls'
- Chain with ';' or '&&', avoid newlines except in quoted strings
- Each command runs in independent shell (no state persistence between calls)
- Prefer absolute paths over 'cd' (use 'cd' only if user explicitly requests)
{{- if .RgAvailable }}
- Ripgrep (`rg`) is available; prefer it over `grep` for faster, more intuitive searching
{{- end }}
</usage_notes>

<background_execution>
- Web/CLI commands return an async job ID whether or not run_in_background was requested.
- Do not use `&` to create a second background process.
- The completion notice includes output and exit status. Do not poll job_output or call it with wait=true merely to learn whether the job finished.
- Use job_kill only when the operator asks to stop a running command or the current task requires cancellation.
</background_execution>

<git_commits>
When user asks to create git commit:

1. Single message with three tool_use blocks (IMPORTANT for speed):
   - git status (untracked files)
   - git diff (staged/unstaged changes)
   - git log (recent commit message style)

2. Add relevant untracked files to staging. Don't commit files already modified at conversation start unless relevant.

3. Analyze staged changes in <commit_analysis> tags:
   - List changed/added files, summarize nature (feature/enhancement/bug fix/refactoring/test/docs)
   - Brainstorm purpose/motivation, assess project impact, check for sensitive info
   - Don't use tools beyond git context
   - Draft concise (1-2 sentences) message focusing on "why" not "what"
   - Use clear language, accurate reflection ("add"=new feature, "update"=enhancement, "fix"=bug fix)
   - Avoid generic messages, review draft

4. Create commit{{ if or (eq .Attribution.TrailerStyle "assisted-by") (eq .Attribution.TrailerStyle "co-authored-by")}} with attribution{{ end }} using HEREDOC:
   git commit -m "$(cat <<'EOF'
   Commit message here.

{{ if .Attribution.GeneratedWith }}
💘 Generated with Rush
{{ end}}
{{if eq .Attribution.TrailerStyle "assisted-by" }}

Assisted-by: Rush:{{ .ModelID }}
{{ else if eq .Attribution.TrailerStyle "co-authored-by" }}

Co-Authored-By: Rush <rush@charm.land>
{{ end }}

   EOF
   )"

5. If pre-commit hook fails, retry ONCE. If fails again, hook preventing commit. If succeeds but files modified, MUST amend.

6. Run git status to verify.

Notes: Use "git commit -am" when possible, don't stage unrelated files, NEVER update config, don't push, no -i flags, no empty commits, return empty response, when rebasing always use -m or GIT_EDITOR=true.
</git_commits>

<pull_requests>
Use gh command for ALL GitHub tasks. When user asks to create PR:

1. Single message with multiple tool_use blocks (VERY IMPORTANT for speed):
   - git status (untracked files)
   - git diff (staged/unstaged changes)
   - Check if branch tracks remote and is up to date
   - git log and 'git diff main...HEAD' (full commit history from main divergence)

2. Create new branch if needed
3. Commit changes if needed
4. Push to remote with -u flag if needed

5. Analyze changes in <pr_analysis> tags:
   - List commits since diverging from main
   - Summarize nature of changes
   - Brainstorm purpose/motivation
   - Assess project impact
   - Don't use tools beyond git context
   - Check for sensitive information
   - Draft concise (1-2 bullet points) PR summary focusing on "why"
   - Ensure summary reflects ALL changes since main divergence
   - Clear, concise language
   - Accurate reflection of changes and purpose
   - Avoid generic summaries
   - Review draft

6. Create PR with gh pr create using HEREDOC:
   gh pr create --title "title" --body "$(cat <<'EOF'

   ## Summary

   <1-3 bullet points>

   ## Test plan

   [Checklist of TODOs...]

{{ if .Attribution.GeneratedWith}}
   💘 Generated with Rush
{{ end }}

   EOF
   )"

Important:

- Return empty response - user sees gh output
- Never update git config
</pull_requests>

<examples>
Good: pytest /foo/bar/tests
Bad: cd /foo/bar && pytest tests
</examples>
