// Package heartbeat maintains a bounded, atomic snapshot of provider activity.
package heartbeat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/procinfo"
)

// Purpose classifies a request stream.
type Purpose string

// Known purposes persisted per model.
const (
	PurposeTurn      Purpose = "turn"
	PurposeTitle     Purpose = "title"
	PurposeSummary   Purpose = "summary"
	PurposeKeepalive Purpose = "keepalive"
	PurposeFetch     Purpose = "fetch"
	PurposePing      Purpose = "ping"
	PurposeOther     Purpose = "other"
)

// Context carries per-call attribution. RootSessionID wins over SessionID.
type Context struct {
	SessionID     string
	RootSessionID string
	AgentID       string
	Purpose       Purpose
	Role, Source  string
}

// Usage is a token/cost delta.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
	CostUSD                              float64
}

type contextKey struct{}

// WithContext attaches heartbeat attribution to ctx.
func WithContext(ctx context.Context, c Context) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// FromContext returns the attribution in ctx, or the zero Context.
func FromContext(ctx context.Context) Context {
	if ctx == nil {
		return Context{}
	}
	c, _ := ctx.Value(contextKey{}).(Context)
	return c
}

var dirFunc atomic.Pointer[func() string]

// launchCwd is captured once at startup.
var launchCwd = func() string { s, _ := os.Getwd(); return s }()

var flushNow = flushDefault

// Test hooks, guarded by RWMutex, never plain variables.
var hooks = struct {
	sync.RWMutex
	now        func() time.Time
	writeFile  func(string, []byte, os.FileMode) error
	renameFile func(string, string) error
	readFile   func(string) ([]byte, error)
}{now: time.Now, writeFile: os.WriteFile, renameFile: os.Rename, readFile: os.ReadFile}

func now() time.Time {
	hooks.RLock()
	f := hooks.now
	hooks.RUnlock()
	return f()
}

func writeFile(p string, b []byte, m os.FileMode) error {
	hooks.RLock()
	f := hooks.writeFile
	hooks.RUnlock()
	return f(p, b, m)
}

func renameFile(a, b string) error {
	hooks.RLock()
	f := hooks.renameFile
	hooks.RUnlock()
	return f(a, b)
}

func readFile(path string) ([]byte, error) {
	hooks.RLock()
	f := hooks.readFile
	hooks.RUnlock()
	return f(path)
}

func setNow(f func() time.Time) { hooks.Lock(); hooks.now = f; hooks.Unlock() }

func setWriteFile(f func(string, []byte, os.FileMode) error) {
	hooks.Lock()
	hooks.writeFile = f
	hooks.Unlock()
}

func setRenameFile(f func(string, string) error) {
	hooks.Lock()
	hooks.renameFile = f
	hooks.Unlock()
}

func setReadFile(f func(string) ([]byte, error)) {
	hooks.Lock()
	hooks.readFile = f
	hooks.Unlock()
}

// registry tracks per-root records by full identity.
var registry = struct {
	sync.Mutex
	rows map[string]*record
}{rows: map[string]*record{}}

// Process identity, cached lazily via sync.Once outside the registry lock.
var (
	processStart, processParent string
	processPID, processPPID     int
	processMetaHook             func()
	processMetaOnce             sync.Once
)

var (
	initWorkspace string
	initSlots     = map[string]string{}
)

var (
	lastFlushAt time.Time
	lastFlushMu sync.Mutex
	flushMu     sync.Mutex
	flushFuncMu sync.RWMutex
)

// workerOnce guarantees a single long-lived worker; workerStarts observes it.
var (
	workerOnce   sync.Once
	workerStarts atomic.Int64
)

// ensureProcessMeta caches pid/ppid/parent/start token exactly once.
func ensureProcessMeta() {
	processMetaOnce.Do(func() {
		if processMetaHook != nil {
			processMetaHook()
		}
		processPID = os.Getpid()
		processPPID = os.Getppid()
		processStart = procinfo.StartToken(processPID)
		processParent = parentName(processPPID)
	})
}

// model is one provider/model entry with totals and per-purpose stats.
type model struct {
	Provider   string                   `json:"provider"`
	Model      string                   `json:"model"`
	Requests   int64                    `json:"requests"`
	Errors     int64                    `json:"errors"`
	LimitHits  int64                    `json:"limit_hits"`
	Input      int64                    `json:"input"`
	Output     int64                    `json:"output"`
	CacheRead  int64                    `json:"cache_read"`
	CacheWrite int64                    `json:"cache_write"`
	CostUSD    float64                  `json:"cost_usd"`
	FirstAt    string                   `json:"first_at"`
	LastAt     string                   `json:"last_at"`
	LastError  string                   `json:"last_error,omitempty"`
	Roles      []string                 `json:"roles"`
	Sources    []string                 `json:"sources"`
	ByPurpose  map[string]*purposeStats `json:"by_purpose"`
}

// purposeStats are counters for one purpose bucket.
type purposeStats struct {
	Requests int64   `json:"requests"`
	Input    int64   `json:"input"`
	Output   int64   `json:"output"`
	CostUSD  float64 `json:"cost_usd"`
}

// agent is a per-caller summary row.
type agent struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Role     string `json:"role"`
	Requests int64  `json:"requests"`
	LastAt   string `json:"last_at"`
}

// record is the on-disk snapshot for one root session.
type record struct {
	Version       int               `json:"v"`
	PID           int               `json:"pid"`
	PPID          int               `json:"ppid"`
	Parent        string            `json:"parent"`
	ProcStart     string            `json:"proc_start"`
	LaunchCwd     string            `json:"launch_cwd"`
	Workspace     string            `json:"workspace"`
	Session       string            `json:"session"`
	State         string            `json:"state"`
	StartedAt     string            `json:"started_at"`
	LastBeat      string            `json:"last_beat"`
	LastRequestAt string            `json:"last_request_at"`
	SlotsAtStart  map[string]string `json:"slots_at_start"`
	Totals        *model            `json:"totals"`
	Models        map[string]*model `json:"models"`
	Agents        []*agent          `json:"agents"`
	identity      string            `json:"-"`
	dirty         bool              `json:"-"`
	generation    uint64            `json:"-"`
}

// SetDirFunc installs a lazily evaluated output directory.
func SetDirFunc(f func() string) {
	if f == nil {
		dirFunc.Store(nil)
		return
	}
	dirFunc.Store(&f)
}

// setFlushForTest replaces the flush entry point; nil restores the default.
func setFlushForTest(f func()) {
	flushFuncMu.Lock()
	if f == nil {
		flushNow = flushDefault
	} else {
		flushNow = func(bool) { f() }
	}
	flushFuncMu.Unlock()
}

func requestFlush(force bool) {
	flushFuncMu.RLock()
	f := flushNow
	flushFuncMu.RUnlock()
	f(force)
}

func flushDefault(force bool) { flush(force) }

// Init supplies workspace and startup slot metadata.
func Init(workspace string, slots map[string]string) {
	ensureProcessMeta()
	registry.Lock()
	defer registry.Unlock()
	initWorkspace = persistString(workspace, 256)
	initSlots = copySlots(slots)
	for _, r := range registry.rows {
		r.Workspace = initWorkspace
		r.SlotsAtStart = copySlots(initSlots)
		r.dirty = true
		r.generation++
	}
}

// bounded truncates by bytes.
func bounded(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// boundedRunes truncates by runes.
func boundedRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// persistString redacts then bounds a string destined for JSON.
func persistString(s string, maxRunes int) string {
	return boundedRunes(RedactError(errors.New(s)), maxRunes)
}

func copySlots(m map[string]string) map[string]string {
	n := map[string]string{}
	for k, v := range m {
		if len(n) >= 8 {
			break
		}
		n[persistString(k, 64)] = persistString(v, 128)
	}
	return n
}

func resolveDir() string {
	if s := os.Getenv("RUSH_HEARTBEAT_DIR"); s != "" {
		return s
	}
	if p := dirFunc.Load(); p != nil {
		return (*p)()
	}
	return ""
}

// rootID is the full snapshot identity: RootSessionID, else SessionID, else _none.
func rootID(c Context) string {
	if c.RootSessionID != "" {
		return c.RootSessionID
	}
	if c.SessionID != "" {
		return c.SessionID
	}
	return "_none"
}

func keyFor(c Context) string { return rootID(c) }

// slug lowercases and keeps only [a-z0-9._-]; everything else becomes "_".
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// filename builds <dirslug>-<h8 dir>--<sessslug>-<h8 identity>--<pid>.json.
// Hashes always cover the FULL launch dir and FULL identity, so distinct ids
// sharing even a long prefix never merge; slugs are display-only prefixes.
func filename(dir, identity string) string {
	dh := sha256.Sum256([]byte(dir))
	d := slug(filepath.Base(filepath.Clean(dir)))
	if len(d) > 40 {
		d = d[:40]
	}
	if d == "" || d[0] == '.' {
		d = "_" + strings.TrimLeft(d, ".")
	}
	s := slug(identity)
	if len(s) > 30 {
		s = s[:30]
	}
	if s == "" || s[0] == '.' {
		s = "_" + strings.TrimLeft(s, ".")
	}
	h := sha256.Sum256([]byte(identity))
	name := fmt.Sprintf("%s-%s--%s-%s--%d.json", d, hex.EncodeToString(dh[:])[:8], s, hex.EncodeToString(h[:])[:8], os.Getpid())
	if len(name) > 120 {
		name = name[:len(name)-(len(name)-120)] // defensive; bounded inputs stay <=120
	}
	return name
}

// fresh builds a new running record using only cached process metadata.
func fresh(c Context) *record {
	t := now().UTC().Format(time.RFC3339)
	return &record{
		Version: 1, PID: processPID, PPID: processPPID, Parent: processParent,
		ProcStart: processStart, LaunchCwd: persistString(launchCwd, 256),
		Workspace: initWorkspace, Session: persistString(keyFor(c), 200), State: "running",
		StartedAt: t, LastBeat: t, SlotsAtStart: copySlots(initSlots),
		Totals: &model{ByPurpose: map[string]*purposeStats{}},
		Models: map[string]*model{}, Agents: []*agent{},
		identity: keyFor(c),
	}
}

func dirtyRows() bool {
	registry.Lock()
	defer registry.Unlock()
	for _, r := range registry.rows {
		if r.dirty {
			return true
		}
	}
	return false
}

func ensureWorker() {
	workerOnce.Do(func() {
		workerStarts.Add(1)
		go worker()
		go func() {
			time.Sleep(100 * time.Millisecond)
			if dirtyRows() {
				requestFlush(true)
			}
		}()
	})
}

// ResetForTest drops every in-memory row and the Init state. Test-only: the
// registry is process-wide, so tests in other packages that reuse a session
// id or run with -count>1 would otherwise accumulate each other's counts.
func ResetForTest() {
	registry.Lock()
	registry.rows = map[string]*record{}
	initWorkspace = ""
	initSlots = map[string]string{}
	registry.Unlock()
}

// resetWorkerForTest re-arms the once so a fresh setup starts a new worker.
func resetWorkerForTest() {
	workerOnce = sync.Once{}
	workerStarts.Store(0)
}

// RecordRequest counts one provider call.
func RecordRequest(ctx context.Context, provider, modelName string, err error) {
	c := FromContext(ctx)
	ensureProcessMeta()
	registry.Lock()
	defer registry.Unlock()
	ensureWorker()
	k := keyFor(c)
	r := registry.rows[k]
	if r == nil {
		r = fresh(c)
		registry.rows[k] = r
	}
	r.State = "running"
	updateRequest(r, c, provider, modelName, err)
}

// AddUsage folds a usage delta into the model and both purpose maps.
func AddUsage(ctx context.Context, provider, modelName string, u Usage) {
	c := FromContext(ctx)
	ensureProcessMeta()
	registry.Lock()
	defer registry.Unlock()
	ensureWorker()
	k := keyFor(c)
	r := registry.rows[k]
	if r == nil {
		r = fresh(c)
		registry.rows[k] = r
	}
	r.State = "running"
	m := getModel(r, provider, modelName)
	m.Input += u.Input
	m.Output += u.Output
	m.CacheRead += u.CacheRead
	m.CacheWrite += u.CacheWrite
	m.CostUSD += u.CostUSD
	r.Totals.Input += u.Input
	r.Totals.Output += u.Output
	r.Totals.CacheRead += u.CacheRead
	r.Totals.CacheWrite += u.CacheWrite
	r.Totals.CostUSD += u.CostUSD
	ps := purpose(c)
	q := ensurePurpose(m.ByPurpose, ps)
	q.Input += u.Input
	q.Output += u.Output
	q.CostUSD += u.CostUSD
	q = ensurePurpose(r.Totals.ByPurpose, ps)
	q.Input += u.Input
	q.Output += u.Output
	q.CostUSD += u.CostUSD
	r.dirty = true
	r.generation++
}

func purpose(c Context) string {
	switch c.Purpose {
	case PurposeTurn, PurposeTitle, PurposeSummary, PurposeKeepalive, PurposeFetch, PurposePing, PurposeOther:
		return string(c.Purpose)
	}
	return string(PurposeOther)
}

func ensurePurpose(m map[string]*purposeStats, k string) *purposeStats {
	if m[k] == nil {
		m[k] = &purposeStats{}
	}
	return m[k]
}

// getModel returns the entry for p/n, folding evicted history into "(other)".
func getModel(r *record, p, n string) *model {
	p = persistString(p, 100)
	n = persistString(n, 100)
	k := p + "/" + n
	if m := r.Models[k]; m != nil {
		return m
	}
	t := now().UTC().Format(time.RFC3339)
	if len(r.Models) >= maxModels {
		m := r.Models[otherKey]
		if m == nil {
			m = &model{Model: otherKey, ByPurpose: map[string]*purposeStats{}}
			for key, v := range r.Models {
				foldModel(m, v)
				delete(r.Models, key)
			}
			r.Models[otherKey] = m
		}
		if m.FirstAt == "" {
			m.FirstAt = t
		}
		m.LastAt = t
		return m
	}
	m := &model{Provider: p, Model: n, FirstAt: t, LastAt: t, ByPurpose: map[string]*purposeStats{}}
	for _, v := range []string{"turn", "title", "summary", "keepalive", "fetch", "ping", "other"} {
		m.ByPurpose[v] = &purposeStats{}
	}
	r.Models[k] = m
	return m
}

const (
	maxModels = 16
	maxAgents = 16
	otherKey  = "(other)"
)

// foldModel adds all of from's counters into to.
func foldModel(to, from *model) {
	if from == nil {
		return
	}
	to.Requests += from.Requests
	to.Errors += from.Errors
	to.LimitHits += from.LimitHits
	to.Input += from.Input
	to.Output += from.Output
	to.CacheRead += from.CacheRead
	to.CacheWrite += from.CacheWrite
	to.CostUSD += from.CostUSD
	if to.FirstAt == "" || from.FirstAt < to.FirstAt {
		to.FirstAt = from.FirstAt
	}
	if from.LastAt > to.LastAt {
		to.LastAt = from.LastAt
		to.LastError = from.LastError
	}
	for k, v := range from.ByPurpose {
		p := ensurePurpose(to.ByPurpose, k)
		p.Requests += v.Requests
		p.Input += v.Input
		p.Output += v.Output
		p.CostUSD += v.CostUSD
	}
	for _, v := range from.Roles {
		addUnique(&to.Roles, v)
	}
	for _, v := range from.Sources {
		addUnique(&to.Sources, v)
	}
}

func updateRequest(r *record, c Context, p, n string, e error) {
	t := now().UTC().Format(time.RFC3339)
	m := getModel(r, p, n)
	m.Requests++
	r.Totals.Requests++
	m.LastAt = t
	r.LastRequestAt = t
	if e != nil {
		m.Errors++
		r.Totals.Errors++
		m.LastError = RedactError(e)
		if isLimit(e.Error()) {
			m.LimitHits++
			r.Totals.LimitHits++
		}
	}
	addUnique(&m.Roles, c.Role)
	addUnique(&m.Sources, c.Source)
	ps := purpose(c)
	ensurePurpose(m.ByPurpose, ps).Requests++
	ensurePurpose(r.Totals.ByPurpose, ps).Requests++
	r.Totals.LastAt = t
	id := persistString(c.AgentID, 200)
	if id != "" {
		for _, a := range r.Agents {
			if a.ID == id {
				a.Requests++
				a.LastAt = t
				a.Model = m.Provider + "/" + m.Model
				a.Role = persistString(c.Role, 100)
				r.dirty = true
				r.generation++
				return
			}
		}
		r.Agents = append(r.Agents, &agent{ID: id, Model: m.Provider + "/" + m.Model, Role: persistString(c.Role, 100), Requests: 1, LastAt: t})
		// Keep the NEWEST maxAgents by last activity.
		sort.Slice(r.Agents, func(i, j int) bool { return r.Agents[i].LastAt > r.Agents[j].LastAt })
		if len(r.Agents) > maxAgents {
			r.Agents = r.Agents[:maxAgents]
		}
	}
	r.dirty = true
	r.generation++
}

func addUnique(a *[]string, s string) {
	s = persistString(s, 100)
	if s == "" {
		return
	}
	for _, v := range *a {
		if v == s {
			return
		}
	}
	*a = append(*a, s)
}

// isLimit classifies provider rate/quota failures.
var limitRE = regexp.MustCompile(`(?i)(\b(status|code|http)\s*[:=]?\s*429\b|too many requests|rate.?limit|quota|usage limit|limit reached)`)

func isLimit(s string) bool { return limitRE.MatchString(s) }

var (
	bearerRE   = regexp.MustCompile(`(?i)bearer\s+[^\s,;]+`)
	basicRE    = regexp.MustCompile(`(?i)\bbasic\s+\S+`)
	keyRE      = regexp.MustCompile(`(?i)("?(?:x-)?(?:api[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password)"?\s*[:=]\s*"?)[^\s"&,;}]+`)
	skRE       = regexp.MustCompile(`(?i)(?:sk-[A-Za-z0-9_-]+|AIza[A-Za-z0-9_-]{20,}|gsk_[A-Za-z0-9_-]+|xai-[A-Za-z0-9_-]+|ghp_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]+)`)
	userinfoRE = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/@]+@`)
	urlRE      = regexp.MustCompile(`(?i)\bhttps?://[^\s]+`)
)

// RedactError removes credential shapes and bounds the result to 200 runes.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	s = bearerRE.ReplaceAllString(s, "Bearer [REDACTED]")
	s = basicRE.ReplaceAllString(s, "Basic [REDACTED]")
	s = keyRE.ReplaceAllString(s, "$1[REDACTED]")
	s = skRE.ReplaceAllString(s, "[REDACTED]")
	s = urlRE.ReplaceAllStringFunc(s, func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			return "[REDACTED_URL]"
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	})
	// Generic scheme userinfo; runs after https stripping so queries go too.
	s = userinfoRE.ReplaceAllString(s, "[REDACTED]@")
	return boundedRunes(s, 200)
}
