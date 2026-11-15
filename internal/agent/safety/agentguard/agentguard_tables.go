package agentguard

// deniedAgents is the canonical denylist of AI-agent CLI binaries.
// Match is case-insensitive and considers .exe/.cmd/.bat/.ps1 suffixes.
// Names that overlap with common shell utilities (e.g. "continue" is also
// a shell keyword) — we accept the false-positive risk because the typical
// agent script does not use bare "continue" as a standalone command.
var deniedAgents = map[string]string{
	// Tier 1 — proprietary heavyweight
	"claude":       "Anthropic Claude Code",
	"codex":        "OpenAI Codex CLI",
	"gemini":       "Google Gemini CLI",
	"qwen":         "Alibaba Qwen Code",
	"qwen-code":    "Alibaba Qwen Code",
	"cody":         "Sourcegraph Cody CLI",
	"windsurf":     "Codeium Windsurf CLI",
	"windsurf-cli": "Codeium Windsurf CLI",

	// Tier 2 — open-source coding agents
	"opencode":     "opencode-ai",
	"aider":        "aider chat",
	"cline":        "Cline (Claude Dev)",
	"cursor-agent": "Cursor Agent CLI",
	"continue":     "Continue.dev CLI",
	"amp":          "Sourcegraph Amp",
	"amp-code":     "Sourcegraph Amp",
	"goose":        "Block Goose",
	"mentat":       "Mentat agent",
	"forge":        "Forge agent",
	"tabby":        "Tabby agent",

	// Tier 3 — us. Blocks any recursive rush/crush invocation regardless of flags.
	"rush":  "this very binary — recursive invocation is never the right answer",
	"crush": "legacy pre-rename binary name for this very binary",
}

// deniedNpmPackages lists packages a sub-agent might launch through npx /
// pnpm dlx / yarn dlx / bunx without the agent binary being on PATH.
var deniedNpmPackages = map[string]string{
	"@anthropic-ai/claude-code": "Anthropic Claude Code (via npx)",
	"@openai/codex":             "OpenAI Codex CLI (via npx)",
	"@google/gemini-cli":        "Google Gemini CLI (via npx)",
	"@opencode-ai/opencode":     "opencode (via npx)",
	"@continue/cli":             "Continue.dev (via npx)",
	"@sourcegraph/amp":          "Sourcegraph Amp (via npx)",
	"@sourcegraph/cody":         "Sourcegraph Cody (via npx)",
	"@cursor-agent/cli":         "Cursor Agent (via npx)",
	"@windsurf/cli":             "Windsurf (via npx)",
	"@qwen-ai/qwen-cli":         "Qwen (via npx)",
}

// deniedPypiPackages: pipx / uvx wrappers.
var deniedPypiPackages = map[string]string{
	"aider-chat": "aider (via pipx/uvx)",
	"aider-cli":  "aider (via pipx/uvx)",
	"mentat-cli": "mentat (via pipx/uvx)",
}

// runners we look INTO — these wrap another command we must re-check.
var packageRunners = map[string]bool{
	"npx":  true,
	"pnpm": true, // pnpm dlx X
	"yarn": true, // yarn dlx X
	"bunx": true,
	"bun":  true, // bun x X
	"pipx": true, // pipx run X
	"uvx":  true,
	"uv":   true, // uv tool run X
}

var shellRunners = map[string]bool{
	"bash":           true,
	"sh":             true,
	"dash":           true,
	"zsh":            true,
	"ksh":            true,
	"fish":           true,
	"cmd":            true,
	"cmd.exe":        true,
	"powershell":     true,
	"powershell.exe": true,
	"pwsh":           true,
	"pwsh.exe":       true,
	"nu":             true, // nushell
}

// commandWrappers are commands that take ANOTHER command as their first
// non-flag argument and execute it. We strip them and re-check what they
// were going to launch. Without this `start claude` / `Start-Process claude`
// / `iex 'claude'` would bypass the denylist.
var commandWrappers = map[string]bool{
	"start":             true, // cmd: start <cmd> [args]
	"start-process":     true, // PowerShell cmdlet
	"start-job":         true, // PowerShell — runs in background but still launches the agent
	"saps":              true, // PowerShell built-in alias for Start-Process
	"sajb":              true, // PowerShell built-in alias for Start-Job
	"invoke-expression": true, // PowerShell: invoke-expression "<string>"
	"iex":               true, // PowerShell alias for invoke-expression
	"invoke-command":    true, // PowerShell remote/local exec
	"icm":               true, // PowerShell alias for invoke-command
}

// windowOpenerVerbs is the subset of commandWrappers that explicitly asks
// Windows for a brand-new, visible console/GUI window — as opposed to
// merely evaluating a string in the current shell (invoke-expression,
// invoke-command). `start`/`Start-Process`/`Start-Job` all map to
// CreateProcess with their OWN creation flags, chosen by cmd.exe/
// PowerShell itself, not by whatever spawned the outer shell — so a
// SysProcAttr.HideWindow set on the outer process (see platform.Command)
// has no effect on the window these verbs create. See
// WindowOpenerError's doc comment for the full mechanism.
var windowOpenerVerbs = map[string]bool{
	"start":         true, // cmd: start <cmd> [args] — always opens a new window
	"start-process": true, // PowerShell cmdlet — same effect, -WindowStyle Hidden not assumed
	"start-job":     true, // PowerShell — background job, but the job's own window still opens
	"saps":          true, // PowerShell built-in alias for Start-Process
	"sajb":          true, // PowerShell built-in alias for Start-Job
}
