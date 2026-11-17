package prompt

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/prompt/skills"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
)

// OrchestratorRuleMarker is the literal heading of rule 7, the block
// coder.md.tpl renders only when WorkerAvailable is true.
// coordinator.sessionPromptForCall (internal/agent/coordinator_session_prompt.go)
// reads a session's STORED system prompt for exactly this substring to tell
// which mode that prompt was built in, so the two must never drift apart: if
// the heading is renamed in the template without this constant following, a
// stored prompt stops advertising its mode and the A24 prompt/tool-set
// reconciliation silently stops working. prompt_test.go pins the pair.
const OrchestratorRuleMarker = "**7. Orchestrator mode.**"

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name       string
	template   string
	now        func() time.Time
	platform   string
	workingDir string

	// gitSnaps holds the last git status snapshot per working directory; see
	// gitStatusSnapshot. gitStatusFn is a test seam (nil means getGitStatus).
	gitMu       sync.Mutex
	gitSnaps    map[string]gitSnapshot
	gitStatusFn func(ctx context.Context, dir string) (string, error)
}

// gitSnapshotTTL bounds how long one git status snapshot is reused. The
// system prompt is rebuilt at every turn start, and the snapshot sits before
// the skills, the project context and the whole conversation: any change in it
// moves the provider's prompt-cache prefix and every later token is re-read at
// full price. Agents edit their worktree between turns, so a live status broke
// the cache on almost every turn start. The prompt labels the block a
// "snapshot", so reusing it for hours loses nothing a turn could rely on.
const gitSnapshotTTL = 6 * time.Hour

type gitSnapshot struct {
	at  time.Time
	val string
}

type PromptDat struct {
	Provider           string
	Model              string
	Config             config.Config
	WorkingDir         string
	IsGitRepo          bool
	Platform           string
	Date               string
	GitStatus          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string

	// WorkerAvailable is true when this run is driven by the Smart
	// model slot AND a Worker model is configured — i.e. exactly the
	// condition coordinator.workerSubAgentActive checks for sub-agents,
	// reused here (not re-derived) for the top-level coder prompt so the
	// two decisions ("does the sub-agent get worker tools/model" and "does
	// the coder get told to delegate") can never disagree. When false, the
	// orchestrator block in coder.md.tpl is entirely absent and the
	// rendered prompt is byte-identical to before this field existed.
	WorkerAvailable bool
	// WorkerContextWindowText is a human-readable size for the configured
	// worker model's context window (e.g. "200k tokens", "1M tokens"),
	// preformatted here rather than in the template because Go's
	// text/template has no arithmetic/formatting pipeline worth the
	// complexity for this. Empty when WorkerAvailable is false, or when
	// it's true but the size is unknown/zero — notably CLI-backed worker
	// models (claude/gemini/qwen via cliprovider) only get a catwalk entry,
	// and therefore a non-zero ContextWindow, when config.Load ran with the
	// CLI binary present on PATH; when the binary wasn't found at load
	// time, GetModel returns nil and this stays "". The template must never
	// render a fabricated number — an empty string here means "omit the
	// number, keep the chunking guidance."
	WorkerContextWindowText string
	// FolderScoped is true when THIS call runs with a folder scope
	// (CallOptions.FolderScope != nil): coder.md.tpl uses it to replace
	// the legacy single-target file-tool guidance with the scoped fs_*
	// batch-tool block. It reaches Build through the context (attached by
	// agent.WithCallOptions at the one place a CallOptions is ever
	// attached, cleared by agent's withoutCallOptions) rather than a
	// Build parameter because the scoped call sites already thread this
	// same ctx and a new positional parameter would have to be touched
	// into every Build caller for a flag only the coder template reads.
	// When false — every legacy caller — the rendered prompt is
	// byte-identical to before this field existed.
	FolderScoped bool
}

type ContextFile struct {
	Path    string
	Content string
}

// folderScopeContextKey carries the per-call folder-scope flag from
// agent.WithCallOptions into Build. The key lives here, not in agent,
// because prompt cannot import agent (agent imports prompt), and Build
// must learn the flag without a new positional parameter.
type folderScopeContextKey struct{}

// WithFolderScoped returns a context carrying the "this call is
// folder-scoped" flag Build renders the coder prompt from. Call it before
// handing the context to Build; absent context or false both render the
// legacy unscoped prompt.
func WithFolderScoped(ctx context.Context, scoped bool) context.Context {
	return context.WithValue(ctx, folderScopeContextKey{}, scoped)
}

// folderScopedFrom reports the flag carried by WithFolderScoped; a context
// without the value reads as false (unscoped).
func folderScopedFrom(ctx context.Context) bool {
	scoped, _ := ctx.Value(folderScopeContextKey{}).(bool)
	return scoped
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:     name,
		template: promptTemplate,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Build renders the prompt template. workerActive should be the caller's
// already-computed "is this run a smart orchestrator with a worker
// configured" predicate (coordinator.workerSubAgentActive for the top-level
// coder; always false for sub-agent/other prompt builds) — Build does not
// re-derive it, so there is exactly one place that decision is made.
// cfg is the PINNED configuration this build must read, passed separately
// from store rather than fetched from it (P1-2 of the 2026-08-18
// release-readiness review). The caller has usually already resolved models
// against one generation of the config; re-reading store.Config() here meant
// a reload landing in between produced a model from generation N and a
// prompt from N+1 -- different context paths, skills, options and Models map
// than the model that will actually run. Making it a parameter forces every
// call site to state which generation it means instead of silently taking
// whatever is current.
//
// store is still needed, but only for process-stable things: WorkingDir()
// and Resolver(). Neither changes with a config reload.
func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore, cfg *config.Config, workerActive bool) (string, error) {
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d, err := p.promptData(ctx, provider, model, store, cfg, workerActive)
	if err != nil {
		return "", err
	}
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return sb.String(), nil
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore, cfg *config.Config, workerActive bool) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	// cfg is the caller's pinned snapshot; store.Config() is deliberately NOT
	// consulted here. See Build's doc for what mixing the two produced. The
	// nil fallback is for callers with nothing to pin (one-shot renders that
	// resolved nothing against an earlier generation).
	if cfg == nil {
		cfg = store.Config()
	}
	contextBudget := contextBudget{}
	contextFiles := loadContextFilesWithBudget(ctx, cfg.Options.ContextPaths, store, &contextBudget)
	if err := ctx.Err(); err != nil {
		return PromptDat{}, err
	}
	globalContextFiles := loadContextFilesWithBudget(ctx, cfg.Options.GlobalContextPaths, store, &contextBudget)
	if err := ctx.Err(); err != nil {
		return PromptDat{}, err
	}

	// Discover and load skills metadata.
	var availSkillXML string

	// Start with builtin skills.
	allSkills := skills.DiscoverBuiltin()
	builtinNames := make(map[string]bool, len(allSkills))
	for _, s := range allSkills {
		builtinNames[s.Name] = true
	}

	// Discover user skills from configured paths.
	if len(cfg.Options.SkillsPaths) > 0 {
		expandedPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
		for _, pth := range cfg.Options.SkillsPaths {
			expandedPaths = append(expandedPaths, expandPath(pth, store))
		}
		for _, userSkill := range skills.Discover(expandedPaths) {
			if builtinNames[userSkill.Name] {
				slog.Warn("User skill overrides builtin skill", "name", userSkill.Name)
			}
			allSkills = append(allSkills, userSkill)
		}
	}

	// Deduplicate: user skills override builtins with the same name.
	allSkills = skills.Deduplicate(allSkills)

	// Filter out disabled skills.
	allSkills = skills.Filter(allSkills, cfg.Options.DisabledSkills)

	if len(allSkills) > 0 {
		availSkillXML = skills.ToPromptXML(allSkills)
	}

	isGit := isGitRepo(store.WorkingDir())
	data := PromptDat{
		Provider:        provider,
		Model:           model,
		Config:          *cfg,
		WorkingDir:      filepath.ToSlash(workingDir),
		IsGitRepo:       isGit,
		Platform:        platform,
		Date:            p.now().Format("1/2/2006"),
		AvailSkillXML:   availSkillXML,
		WorkerAvailable: workerActive,
		FolderScoped:    folderScopedFrom(ctx),
	}
	if workerActive {
		if workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]; ok {
			if m := cfg.GetModel(workerModelCfg.Provider, workerModelCfg.Model); m != nil && m.ContextWindow > 0 {
				data.WorkerContextWindowText = formatTokenCount(m.ContextWindow)
			}
		}
	}
	if isGit {
		var err error
		data.GitStatus, err = p.gitStatusSnapshot(ctx, store.WorkingDir())
		if err != nil {
			return PromptDat{}, err
		}
	}

	data.ContextFiles = dedupeContextFiles(flattenContextFiles(contextFiles))
	data.GlobalContextFiles = dedupeContextFiles(flattenContextFiles(globalContextFiles))
	return data, nil
}

// formatTokenCount renders a token count the way a model expects to read it
// in prose ("200k tokens", "1M tokens") rather than a raw integer. Only
// exact, evenly-divisible thousands/millions get the short suffix so we
// never silently round away precision the model might reasonably want;
// anything else falls back to a plain decimal count. No reusable formatter
// for this existed in a package internal/agent can import (internal/cmd has
// the inverse parser, parseTokenCount, but internal/agent must not import
// internal/cmd), so this is a small local helper rather than a new shared
// dependency.
func formatTokenCount(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n%1_000_000 == 0:
		return fmt.Sprintf("%dM tokens", n/1_000_000)
	case n%1_000 == 0:
		return fmt.Sprintf("%dk tokens", n/1_000)
	default:
		return fmt.Sprintf("%d tokens", n)
	}
}

// gitStatusSnapshot returns the git status block for dir, reusing the previous
// one for gitSnapshotTTL. A failed read is returned and not cached.
func (p *Prompt) gitStatusSnapshot(ctx context.Context, dir string) (string, error) {
	p.gitMu.Lock()
	defer p.gitMu.Unlock()
	now := p.now()
	if s, ok := p.gitSnaps[dir]; ok && now.Sub(s.at) < gitSnapshotTTL {
		return s.val, nil
	}
	read := p.gitStatusFn
	if read == nil {
		read = getGitStatus
	}
	val, err := read(ctx, dir)
	if err != nil {
		return "", err
	}
	if p.gitSnaps == nil {
		p.gitSnaps = make(map[string]gitSnapshot)
	}
	p.gitSnaps[dir] = gitSnapshot{at: now, val: val}
	return val, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func getGitStatus(ctx context.Context, dir string) (string, error) {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch, err := getGitBranch(ctx, sh)
	if err != nil {
		return "", err
	}
	status, err := getGitStatusSummary(ctx, sh)
	if err != nil {
		return "", err
	}
	commits, err := getGitRecentCommits(ctx, sh)
	if err != nil {
		return "", err
	}
	return branch + status + commits, nil
}

func getGitBranch(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git branch --show-current 2>/dev/null")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	return fmt.Sprintf("Current branch: %s\n", out), nil
}

func getGitStatusSummary(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git status --short 2>/dev/null | head -20")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n", nil
	}
	return fmt.Sprintf("Status:\n%s\n", out), nil
}

func getGitRecentCommits(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git log --oneline -n 3 2>/dev/null")
	if err != nil || out == "" {
		return "", nil
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out), nil
}

func (p *Prompt) Name() string {
	return p.name
}
