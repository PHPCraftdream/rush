package prompt

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/home"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/PHPCraftdream/rush/internal/skills"
)

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name       string
	template   string
	now        func() time.Time
	platform   string
	workingDir string
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

const (
	// These limits bound prompt construction before template rendering and
	// apply to project and global context together.
	maxContextFileBytes = 64 * 1024
	maxContextFiles     = 128
	maxContextBytes     = 512 * 1024
	maxContextEntries   = 4096
	maxContextDepth     = 64
	maxContextPathBytes = 16 * 1024
	contextReadDirChunk = 64
)

type contextBudget struct {
	files   int
	bytes   int
	entries int
}

type contextDirectory interface {
	ReadDir(n int) ([]os.DirEntry, error)
	Close() error
}

// contextFile is deliberately small so tests can provide a reader whose
// Close unblocks a pending Read. Implementations must honor that contract:
// cancellation closes the owned handle and joins the read/stat goroutine
// before returning, so no detached goroutine is permitted.
type contextFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type contextFileOpener func(path string) (contextFile, error)

func openContextFile(path string) (contextFile, error) {
	return os.Open(path)
}

type contextFileOpenerKey struct{}

type contextDirectoryOpener func(path string) (contextDirectory, error)

type contextDirectoryOpenerKey struct{}

type contextFileInspector func(path string) bool

type contextFileInspectorKey struct{}

func withContextFileOpener(ctx context.Context, opener contextFileOpener) context.Context {
	return context.WithValue(ctx, contextFileOpenerKey{}, opener)
}

func contextFileOpenerFrom(ctx context.Context) contextFileOpener {
	if opener, ok := ctx.Value(contextFileOpenerKey{}).(contextFileOpener); ok && opener != nil {
		return opener
	}
	return openContextFile
}

func withContextDirectoryOpener(ctx context.Context, opener contextDirectoryOpener) context.Context {
	return context.WithValue(ctx, contextDirectoryOpenerKey{}, opener)
}

func contextDirectoryOpenerFrom(ctx context.Context) contextDirectoryOpener {
	if opener, ok := ctx.Value(contextDirectoryOpenerKey{}).(contextDirectoryOpener); ok && opener != nil {
		return opener
	}
	return func(path string) (contextDirectory, error) { return os.Open(path) }
}

func withContextFileInspector(ctx context.Context, inspector contextFileInspector) context.Context {
	return context.WithValue(ctx, contextFileInspectorKey{}, inspector)
}

func contextFileInspectorFrom(ctx context.Context) contextFileInspector {
	if inspector, ok := ctx.Value(contextFileInspectorKey{}).(contextFileInspector); ok && inspector != nil {
		return inspector
	}
	return absoluteContextFileAllowed
}

// readContextChunk joins the reader goroutine before returning. Closing the
// owned handle on cancellation makes the cancellation path bounded without
// leaving a detached goroutine behind.
func readContextChunk(ctx context.Context, f contextFile, dst []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	readDone := make(chan result, 1)
	go func() {
		n, err := f.Read(dst)
		readDone <- result{n: n, err: err}
	}()

	select {
	case result := <-readDone:
		return result.n, result.err
	case <-ctx.Done():
		_ = f.Close()
		<-readDone
		return 0, ctx.Err()
	}
}

func statContextFile(ctx context.Context, f contextFile) (os.FileInfo, error) {
	type result struct {
		info os.FileInfo
		err  error
	}
	statDone := make(chan result, 1)
	go func() {
		info, err := f.Stat()
		statDone <- result{info: info, err: err}
	}()

	select {
	case result := <-statDone:
		return result.info, result.err
	case <-ctx.Done():
		_ = f.Close()
		<-statDone
		return nil, ctx.Err()
	}
}

func processFile(filePath string) *ContextFile {
	budget := contextBudget{}
	return readContextFile(context.Background(), filePath, &budget)
}

func processContextPath(p string, store *config.ConfigStore) []ContextFile {
	return processContextPathWithBudget(context.Background(), p, store, &contextBudget{})
}

func processContextPathWithBudget(ctx context.Context, p string, store *config.ConfigStore, budget *contextBudget) []ContextFile {
	// Unreadable, nonregular, escaping, duplicate-by-path, and over-limit
	// files are skipped without partial content. Cancellation aborts Build
	// with ctx.Err instead of rendering a partial context set.
	if budget.files >= maxContextFiles || budget.bytes >= maxContextBytes {
		return nil
	}
	fullPath, trusted, err := contextPath(p, store.WorkingDir())
	if err != nil {
		slog.Debug("Skipping context path", "path", p, "error", err)
		return nil
	}
	if !trusted {
		return processRelativeContextPath(ctx, fullPath, store.WorkingDir(), budget)
	}

	if err := ctx.Err(); err != nil {
		return nil
	}
	if !budget.visit(fullPath) {
		return nil
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil
	}
	targetInfo, err := os.Stat(fullPath)
	if err != nil {
		return nil
	}
	if targetInfo.IsDir() {
		if info.Mode()&os.ModeSymlink != 0 || isContextReparsePoint(fullPath) {
			return nil
		}
		dir, err := contextDirectoryOpenerFrom(ctx)(fullPath)
		if err != nil {
			return nil
		}
		return walkContextDirectory(ctx, dir, fullPath, fullPath, 0, budget, contextDirectoryOpenerFrom(ctx), contextFileOpenerFrom(ctx), contextFileInspectorFrom(ctx))
	}
	if !targetInfo.Mode().IsRegular() {
		return nil
	}
	return contextResult(readContextFileWithOpener(ctx, fullPath, budget, contextFileOpenerFrom(ctx)))
}

func processRelativeContextPath(ctx context.Context, fullPath, workingDir string, budget *contextBudget) []ContextFile {
	rootPath, err := filepath.Abs(workingDir)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(rootPath, fullPath)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	if !budget.visit(fullPath) {
		return nil
	}
	root, err := openContextRoot(rootPath)
	if err != nil {
		return nil
	}
	defer root.Close()
	rel = filepath.ToSlash(rel)
	info, err := root.Lstat(rel)
	if err != nil {
		return nil
	}
	targetInfo, err := root.Stat(rel)
	if err != nil {
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if targetInfo.IsDir() {
			return nil
		}
	}
	if !targetInfo.IsDir() {
		if !targetInfo.Mode().IsRegular() {
			return nil
		}
		entry, err := root.Open(rel)
		if err != nil {
			return nil
		}
		return contextResult(readContextFileOpened(ctx, fullPath, entry, budget))
	}
	entry, err := root.Open(rel)
	if err != nil {
		return nil
	}
	return walkContextDirectory(ctx, entry, rel, fullPath, 0, budget, func(path string) (contextDirectory, error) {
		return root.Open(path)
	}, func(path string) (contextFile, error) {
		return root.Open(path)
	}, func(path string) bool { return rootedContextFileAllowed(root, path) })
}

func walkContextDirectory(
	ctx context.Context,
	dir contextDirectory,
	dirToken, displayDir string,
	depth int,
	budget *contextBudget,
	openDir func(string) (contextDirectory, error),
	openFile func(string) (contextFile, error),
	inspectFile func(string) bool,
) []ContextFile {
	defer dir.Close()
	if depth >= maxContextDepth {
		return nil
	}
	entries := make([]os.DirEntry, 0, min(contextReadDirChunk, maxContextEntries-budget.entries))
	for len(entries) < maxContextEntries-budget.entries {
		if err := ctx.Err(); err != nil {
			return nil
		}
		remaining := maxContextEntries - budget.entries - len(entries)
		if remaining <= 0 {
			break
		}
		chunkSize := min(contextReadDirChunk, remaining)
		chunk, err := dir.ReadDir(chunkSize)
		entries = append(entries, chunk...)
		if err != nil {
			if err != io.EOF {
				slog.Debug("Stopping context directory traversal", "path", displayDir, "error", err)
			}
			break
		}
		if len(chunk) == 0 {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var contexts []ContextFile
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil
		}
		path := joinContextToken(dirToken, entry.Name())
		displayPath := joinContextDisplay(displayDir, entry.Name())
		if !budget.visit(displayPath) {
			return contexts
		}
		if entry.IsDir() {
			if depth+1 >= maxContextDepth || isContextReparsePoint(displayPath) {
				continue
			}
			subdir, err := openDir(path)
			if err != nil {
				continue
			}
			contexts = append(contexts, walkContextDirectory(ctx, subdir, path, displayPath, depth+1, budget, openDir, openFile, inspectFile)...)
			continue
		}
		if !inspectFile(path) {
			continue
		}
		file, err := openFile(path)
		if err != nil {
			continue
		}
		if result := readContextFileOpened(ctx, displayPath, file, budget); result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

func absoluteContextFileAllowed(path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return false
	}
	targetInfo, err := os.Stat(path)
	return err == nil && targetInfo.Mode().IsRegular()
}

func rootedContextFileAllowed(root *os.Root, path string) bool {
	if _, err := root.Lstat(path); err != nil {
		return false
	}
	targetInfo, err := root.Stat(path)
	return err == nil && targetInfo.Mode().IsRegular()
}

func (b *contextBudget) visit(path string) bool {
	if b.entries >= maxContextEntries {
		return false
	}
	b.entries++
	return len(path) <= maxContextPathBytes
}

func joinContextToken(dir, name string) string {
	if filepath.IsAbs(dir) {
		return filepath.Join(dir, name)
	}
	return filepath.ToSlash(filepath.Join(filepath.FromSlash(dir), name))
}

func joinContextDisplay(dir, name string) string {
	return filepath.Join(dir, name)
}

func contextResult(file *ContextFile) []ContextFile {
	if file == nil {
		return nil
	}
	return []ContextFile{*file}
}

func openContextRoot(path string) (*os.Root, error) {
	for attempt := 0; attempt < 3; attempt++ {
		rootInfo, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(path)
		if err != nil {
			continue
		}
		openedInfo, err := root.Stat(".")
		if err == nil && os.SameFile(rootInfo, openedInfo) {
			return root, nil
		}
		_ = root.Close()
	}
	return nil, fmt.Errorf("working directory changed while opening context root")
}

func contextPath(path, workingDir string) (fullPath string, trusted bool, err error) {
	if filepathext.SmartIsAbs(path) {
		fullPath, err = filepath.Abs(path)
		return fullPath, true, err
	}
	if filepath.VolumeName(path) != "" || hasParentPathElement(path) {
		return "", false, fmt.Errorf("relative context path is not project-relative")
	}
	root, err := filepath.Abs(workingDir)
	if err != nil {
		return "", false, err
	}
	return filepath.Join(root, path), false, nil
}

func hasParentPathElement(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func readContextFile(ctx context.Context, filePath string, budget *contextBudget) *ContextFile {
	return readContextFileWithOpener(ctx, filePath, budget, contextFileOpenerFrom(ctx))
}

func readContextFileWithOpener(ctx context.Context, filePath string, budget *contextBudget, opener contextFileOpener) *ContextFile {
	if err := ctx.Err(); err != nil || budget.files >= maxContextFiles {
		return nil
	}
	remaining := maxContextBytes - budget.bytes
	if remaining <= 0 {
		return nil
	}
	f, err := opener(filePath)
	if err != nil {
		return nil
	}
	return readContextFileOpened(ctx, filePath, f, budget)
}

func readContextFileOpened(ctx context.Context, filePath string, f contextFile, budget *contextBudget) *ContextFile {
	if err := ctx.Err(); err != nil || budget.files >= maxContextFiles {
		_ = f.Close()
		return nil
	}
	remaining := maxContextBytes - budget.bytes
	if remaining <= 0 {
		_ = f.Close()
		return nil
	}
	limit := min(maxContextFileBytes, remaining)
	defer f.Close()
	info, err := statContextFile(ctx, f)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > int64(limit) {
		return nil
	}

	data := make([]byte, limit+1)
	n := 0
	for n < len(data) {
		readN, readErr := readContextChunk(ctx, f, data[n:])
		n += readN
		if readErr != nil && readErr != io.EOF {
			return nil
		}
		if n > limit {
			return nil
		}
		if n > int(info.Size()) {
			return nil
		}
		if readErr == io.EOF {
			break
		}
		if readN == 0 {
			if n == int(info.Size()) {
				break
			}
			return nil
		}
	}
	if n > limit || n != int(info.Size()) {
		return nil
	}
	// A regular file can grow after Stat. Probe for one more byte when the
	// advertised size did not fill the bounded buffer.
	if n < len(data) && n == int(info.Size()) {
		readN, readErr := readContextChunk(ctx, f, data[n:n+1])
		if readN > 0 || (readErr != nil && readErr != io.EOF) {
			return nil
		}
	}
	if n > limit {
		return nil
	}
	budget.files++
	budget.bytes += n
	return &ContextFile{Path: filePath, Content: string(data[:n])}
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of paths.
func loadContextFiles(paths []string, store *config.ConfigStore) map[string][]ContextFile {
	return loadContextFilesWithBudget(context.Background(), paths, store, &contextBudget{})
}

func loadContextFilesWithBudget(ctx context.Context, paths []string, store *config.ConfigStore, budget *contextBudget) map[string][]ContextFile {
	files := map[string][]ContextFile{}
	for _, pth := range paths {
		if ctx.Err() != nil {
			return files
		}
		expanded := expandPath(pth, store)
		pathKey := strings.ToLower(expanded)
		if _, ok := files[pathKey]; ok {
			continue
		}
		files[pathKey] = processContextPathWithBudget(ctx, expanded, store, budget)
	}
	return files
}

// flattenContextFiles collects a path-keyed context-file map into a single
// slice, sorted by path key for deterministic ordering — map iteration order
// in Go is randomized, and dedupeContextFiles below needs a stable "first
// occurrence wins" rule to produce the same result on every run.
func flattenContextFiles(byPath map[string][]ContextFile) []ContextFile {
	keys := make([]string, 0, len(byPath))
	for k := range byPath {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []ContextFile
	for _, k := range keys {
		out = append(out, byPath[k]...)
	}
	return out
}

// dedupeContextFiles drops files whose content is byte-identical to a file
// already kept. Popular coding agents each look for their own instruction
// file (CLAUDE.md, AGENTS.md, GEMINI.md, ...; see defaultContextPaths in
// internal/config/config.go), so a project that keeps several of them in
// sync — often literal copies or symlinks of one another — would otherwise
// have the same instructions injected into the prompt multiple times,
// wasting context for no benefit. The first occurrence, in the deterministic
// order produced by flattenContextFiles, wins; later duplicates are dropped
// entirely (not just blanked), so their <file path="..."> wrapper doesn't
// appear either.
func dedupeContextFiles(files []ContextFile) []ContextFile {
	seen := make(map[[sha256.Size]byte]bool, len(files))
	out := make([]ContextFile, 0, len(files))
	for _, f := range files {
		hash := sha256.Sum256([]byte(f.Content))
		if seen[hash] {
			slog.Debug("prompt: skipping duplicate context file content", "path", f.Path)
			continue
		}
		seen[hash] = true
		out = append(out, f)
	}
	return out
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
		data.GitStatus, err = getGitStatus(ctx, store.WorkingDir())
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
