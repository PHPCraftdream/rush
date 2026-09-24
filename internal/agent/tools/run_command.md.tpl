Run ONE program with an argument array. This is NOT a shell.

<no_shell>
There is no shell involved — the program and arguments reach the OS process-creation call exactly as given:
- `;`, `&&`, `||`, `|` (pipes) are NOT interpreted
- `$(...)`, backticks, `$VAR` are NOT expanded
- `*`, `?`, globs are NOT expanded
- `>`, `<`, redirection is NOT interpreted

To use an env var's value or a glob expansion, resolve it yourself first (read the value, list the files with grep/glob) and pass literal strings.
</no_shell>

<no_builtins>
Because there is no shell, shell builtins CANNOT run: echo, cd, test, set, export, alias, and friends are unavailable. Only real executables found via PATH.
</no_builtins>

<behavior_notes>
- In web and CLI sessions, the program starts asynchronously and returns a job ID; its output and exit status arrive as a new session message. Continue independent work instead of waiting or polling. SDK calls retain foreground behavior.
- Bounded program timeout: default {{ .DefaultTimeout }}s, maximum {{ .MaxTimeout }}s (larger values are clamped).
- Programs on the shared block list cannot run: {{ .BannedCommands }} — plus package-manager install patterns like `go install`, `npm install --global`, `go test -exec`.
- `working_dir` must stay inside the current working directory.
- Output is truncated at {{ .MaxOutputLength }} characters.
- No shell-expansion layer means faster startup than the bash tool on Windows (no MSYS layer).
</behavior_notes>
