// `rush ps`: the operator's view of the global heartbeat directory — which
// process, in which folder, in which session is using which model right now.
package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/spf13/cobra"
)

const (
	// psWatchInterval is the --watch redraw period.
	psWatchInterval = 2 * time.Second
	// psClearScreen is a plain ANSI home+clear (no TUI dependency).
	psClearScreen = "\x1b[H\x1b[2J\x1b[3J"
)

var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "Show which rush processes are using which models right now",
	Long: `Read the global heartbeat directory and print one row per rush
process: its state, the model it used last (plus +N for others), the roles
that drove it, its session, launch dir, pid, and request/token/cost totals.
Includes hidden consumers (title/summary/keepalive/fetch/ping), with an
indented per-purpose split under each row that used more than one purpose.

Rows show STALE-MODEL when a live process uses a model no current slot
(smart/fast/worker/reviewer) points to, with the cause: slot@start (the
slot changed after this process started), per-call (call-site source), or
session-override.

By default only live rows are shown, plus rows that stopped within the
last 10 minutes; --all also shows stopped and stale rows. --json emits
entries, a per-model summary, and the current slots. --watch redraws
every 2 seconds until Ctrl+C.

This command reads only the global heartbeat directory and the global
settings file; it never opens the sessions DB or requires a workspace.`,
	Example: `
rush ps                    # live processes and their models
rush ps --all              # include stopped and stale rows
rush ps --model glm        # only rows touching a model matching "glm"
rush ps --json             # machine-readable: entries, summary, slots
rush ps --watch            # redraw every 2s until Ctrl+C
  `,
	RunE: psRunE,
}

func init() {
	psCmd.Flags().Bool("watch", false, "Redraw every 2 seconds until Ctrl+C")
	psCmd.Flags().Bool("json", false, "Emit stable machine-readable JSON")
	psCmd.Flags().String("model", "", "Case-insensitive substring filter on provider/model")
	psCmd.Flags().Bool("all", false, "Include stopped and stale rows")
}

// psHeartbeatDir resolves the global heartbeat directory: RUSH_HEARTBEAT_DIR
// wins, else the heartbeat/ folder next to the global settings file.
func psHeartbeatDir() string {
	if d := os.Getenv("RUSH_HEARTBEAT_DIR"); d != "" {
		return d
	}
	return filepath.Join(filepath.Dir(config.GlobalConfigData()), "heartbeat")
}

func psRunE(cmd *cobra.Command, _ []string) error {
	watch, _ := cmd.Flags().GetBool("watch")
	asJSON, _ := cmd.Flags().GetBool("json")
	modelFilter, _ := cmd.Flags().GetString("model")
	showAll, _ := cmd.Flags().GetBool("all")
	if watch && asJSON {
		return errors.New("--watch cannot be combined with --json")
	}
	// ReadAll resolves the directory through the package dir func; ps wires
	// it to the global location itself because it never runs the app setup
	// that would normally do so.
	heartbeat.SetDirFunc(psHeartbeatDir)
	load := func() ([]psRow, int, psSlots, error) {
		entries, err := heartbeat.ReadAll()
		if err != nil {
			return nil, 0, psSlots{}, err
		}
		now := time.Now()
		slots := readPsSlots()
		visible, hidden := filterPsRows(buildPsRows(entries, slots, now), now, showAll, modelFilter)
		sortPsRows(visible)
		return visible, hidden, slots, nil
	}
	if asJSON {
		rows, _, slots, err := load()
		if err != nil {
			return fmt.Errorf("read heartbeat snapshots: %w", err)
		}
		return psWriteJSON(rows, slots)
	}
	frame := func() (string, error) {
		rows, hidden, slots, err := load()
		if err != nil {
			return "", err
		}
		return psFrameText(rows, hidden, slots, time.Now()), nil
	}
	if !watch {
		out, err := frame()
		if err != nil {
			return fmt.Errorf("read heartbeat snapshots: %w", err)
		}
		fmt.Print(out)
		return nil
	}
	return psWatchFrames(cmd.Context(), frame)
}

// psWatchFrames clears and reprints one frame every psWatchInterval until
// Ctrl+C or the command context is done.
func psWatchFrames(ctx context.Context, frame func() (string, error)) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	ticker := time.NewTicker(psWatchInterval)
	defer ticker.Stop()
	for {
		out, err := frame()
		if err != nil {
			return err
		}
		fmt.Print(psClearScreen)
		fmt.Print(out)
		select {
		case <-sig:
			return nil
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// psRow is one rendered process line derived from a heartbeat entry.
type psRow struct {
	entry       heartbeat.Entry
	state       string
	beatAgeSec  int64
	modelKey    string
	extraModels int
	roles       []string
	requests    int64
	tokens      int64
	costUSD     float64
	stale       []psStaleModel
	purposes    []psPurpose
	lastActive  time.Time
}

// psPurpose is one per-purpose split line (shown only when >1 has requests).
type psPurpose struct {
	Name     string
	Requests int64
	Tokens   int64
	CostUSD  float64
}

// psModelKey renders the lowercased "provider/model" identity used here.
func psModelKey(provider, model string) string {
	return strings.ToLower(provider + "/" + model)
}

// psModelKeysByRecency returns an entry's model keys, most recently used
// first, with the key as a stable tiebreak.
func psModelKeysByRecency(e heartbeat.Entry) []string {
	keys := slices.Sorted(maps.Keys(e.Models))
	slices.SortStableFunc(keys, func(a, b string) int {
		if c := psParseTime(e.Models[b].LastAt).Compare(psParseTime(e.Models[a].LastAt)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return keys
}

// buildPsRows derives display rows from raw snapshot entries.
func buildPsRows(entries []heartbeat.Entry, slots psSlots, now time.Time) []psRow {
	rows := make([]psRow, 0, len(entries))
	for _, e := range entries {
		r := psRow{
			entry:      e,
			state:      psRowState(e, now),
			beatAgeSec: max64(0, e.BeatAgeSec),
		}
		if keys := psModelKeysByRecency(e); len(keys) > 0 {
			m := e.Models[keys[0]]
			r.modelKey = psModelKey(m.Provider, m.Model)
			r.roles = slices.Clone(m.Roles)
			r.extraModels = len(keys) - 1
		}
		if t := e.Totals; t != nil {
			r.requests = t.Requests
			r.tokens = t.Input + t.Output
			r.costUSD = t.CostUSD
			r.purposes = psPurposeLines(e)
		}
		r.stale = psStaleModels(e, slots)
		r.lastActive = psRowLastActive(e)
		rows = append(rows, r)
	}
	return rows
}

// psPurposeLines collects the row-level purpose split (requests > 0 only),
// sorted by name for stable output.
func psPurposeLines(e heartbeat.Entry) []psPurpose {
	out := []psPurpose{}
	if e.Totals == nil {
		return out
	}
	for name, p := range e.Totals.ByPurpose {
		if p == nil || p.Requests <= 0 {
			continue
		}
		out = append(out, psPurpose{
			Name:     name,
			Requests: p.Requests,
			Tokens:   p.Input + p.Output,
			CostUSD:  p.CostUSD,
		})
	}
	slices.SortFunc(out, func(a, b psPurpose) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// psRowLastActive is the row's most recent activity timestamp.
func psRowLastActive(e heartbeat.Entry) time.Time {
	best := time.Time{}
	for _, s := range []string{e.LastRequestAt, e.LastBeat, e.StartedAt} {
		if t := psParseTime(s); t.After(best) {
			best = t
		}
	}
	return best
}

// filterPsRows returns the rows to show: by default live rows plus rows
// stopped within the last 10 minutes (--all shows everything), optionally
// narrowed by a case-insensitive --model substring on provider/model.
// hidden counts rows dropped only by the default stopped/stale rule.
func filterPsRows(rows []psRow, now time.Time, showAll bool, modelSubstr string) (visible []psRow, hidden int) {
	sub := strings.ToLower(strings.TrimSpace(modelSubstr))
	visible = []psRow{}
	for _, r := range rows {
		if !showAll && !psRowVisibleByDefault(r, now) {
			hidden++
			continue
		}
		if sub != "" && !psRowMatchesModel(r, sub) {
			continue
		}
		visible = append(visible, r)
	}
	return visible, hidden
}

// psRowVisibleByDefault reports whether a row shows without --all.
func psRowVisibleByDefault(r psRow, now time.Time) bool {
	switch r.state {
	case "running", "idle":
		return true
	case "stopped":
		t := psParseTime(r.entry.LastBeat)
		if t.IsZero() {
			t = psParseTime(r.entry.StartedAt)
		}
		return !t.IsZero() && now.Sub(t) <= psStoppedShowWindow
	}
	return false
}

// psRowMatchesModel reports whether any of the row's model keys contains
// the lowercased substring.
func psRowMatchesModel(r psRow, sub string) bool {
	for key := range r.entry.Models {
		if strings.Contains(strings.ToLower(key), sub) {
			return true
		}
	}
	return false
}

// sortPsRows orders rows most recently active first, with stable tiebreaks.
func sortPsRows(rows []psRow) {
	slices.SortStableFunc(rows, func(a, b psRow) int {
		if c := b.lastActive.Compare(a.lastActive); c != 0 {
			return c
		}
		if c := strings.Compare(a.entry.Session, b.entry.Session); c != 0 {
			return c
		}
		return cmp.Compare(a.entry.PID, b.entry.PID)
	})
}

// Stable --json shapes: only what the snapshot already holds, plus the
// derived state, beat age, and stale-model findings.
type psJSONDoc struct {
	SlotsKnown bool                    `json:"slots_known"`
	Slots      map[string]*psSlotModel `json:"slots"`
	Entries    []psJSONEntry           `json:"entries"`
	Summary    []psJSONSummary         `json:"summary"`
}

type psJSONEntry struct {
	PID           int               `json:"pid"`
	PPID          int               `json:"ppid"`
	Parent        string            `json:"parent"`
	State         string            `json:"state"`
	Session       string            `json:"session"`
	LaunchDir     string            `json:"launch_dir"`
	Workspace     string            `json:"workspace"`
	StartedAt     string            `json:"started_at"`
	LastBeat      string            `json:"last_beat"`
	LastRequestAt string            `json:"last_request_at"`
	BeatAgeSec    int64             `json:"beat_age_sec"`
	SlotsAtStart  map[string]string `json:"slots_at_start,omitempty"`
	Totals        psJSONUsage       `json:"totals"`
	Models        []psJSONModel     `json:"models"`
	Agents        []psJSONAgent     `json:"agents"`
	StaleModels   []psStaleModel    `json:"stale_models"`
}

type psJSONUsage struct {
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	LimitHits  int64   `json:"limit_hits"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	CostUSD    float64 `json:"cost_usd"`
}

type psJSONModel struct {
	Provider   string             `json:"provider"`
	Model      string             `json:"model"`
	Requests   int64              `json:"requests"`
	Errors     int64              `json:"errors"`
	LimitHits  int64              `json:"limit_hits"`
	Input      int64              `json:"input"`
	Output     int64              `json:"output"`
	CacheRead  int64              `json:"cache_read"`
	CacheWrite int64              `json:"cache_write"`
	CostUSD    float64            `json:"cost_usd"`
	FirstAt    string             `json:"first_at"`
	LastAt     string             `json:"last_at"`
	LastError  string             `json:"last_error,omitempty"`
	Roles      []string           `json:"roles,omitempty"`
	Sources    []string           `json:"sources,omitempty"`
	Purposes   map[string]psUsage `json:"purposes"`
}

type psJSONAgent struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Role     string `json:"role"`
	Requests int64  `json:"requests"`
	LastAt   string `json:"last_at"`
}

// psUsage is the stable shape of a usage/counter bucket.
type psUsage struct {
	Requests int64   `json:"requests"`
	Input    int64   `json:"input"`
	Output   int64   `json:"output"`
	CostUSD  float64 `json:"cost_usd"`
}

type psJSONSummary struct {
	Model     string  `json:"model"`
	Live      int     `json:"live"`
	Entries   int     `json:"entries"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	LimitHits int64   `json:"limit_hits"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CostUSD   float64 `json:"cost_usd"`
	LastAt    string  `json:"last_at"`
}

// psWriteJSON emits the stable machine-readable report on stdout.
func psWriteJSON(rows []psRow, slots psSlots) error {
	doc := psJSONDoc{SlotsKnown: slots.known, Entries: []psJSONEntry{}, Summary: []psJSONSummary{}}
	if slots.known {
		doc.Slots = map[string]*psSlotModel{}
		for _, name := range psSlotNames {
			if m, ok := slots.bySlot[name]; ok {
				v := m
				doc.Slots[name] = &v
			} else {
				doc.Slots[name] = nil
			}
		}
	}
	for _, r := range rows {
		doc.Entries = append(doc.Entries, psJSONEntryFromRow(r))
	}
	for _, s := range psSummaryRows(rows) {
		doc.Summary = append(doc.Summary, psJSONSummary{
			Model:     s.Model,
			Live:      s.Live,
			Entries:   s.Entries,
			Requests:  s.Requests,
			Errors:    s.Errors,
			LimitHits: s.LimitHits,
			Input:     s.Input,
			Output:    s.Output,
			CostUSD:   s.CostUSD,
			LastAt:    psFormatRFC3339(s.LastAt),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// psFormatRFC3339 renders a time as UTC RFC3339, or "" when zero.
func psFormatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// psJSONEntryFromRow converts one derived row into its stable JSON shape.
// Field access on the snapshot's unexported member types happens inline so
// the types never need naming here.
func psJSONEntryFromRow(r psRow) psJSONEntry {
	e := r.entry
	out := psJSONEntry{
		PID:           e.PID,
		PPID:          e.PPID,
		Parent:        e.Parent,
		State:         r.state,
		Session:       e.Session,
		LaunchDir:     e.LaunchCwd,
		Workspace:     e.Workspace,
		StartedAt:     e.StartedAt,
		LastBeat:      e.LastBeat,
		LastRequestAt: e.LastRequestAt,
		BeatAgeSec:    r.beatAgeSec,
		SlotsAtStart:  e.SlotsAtStart,
		Totals:        psJSONUsage{},
		Models:        []psJSONModel{},
		Agents:        []psJSONAgent{},
		StaleModels:   r.stale,
	}
	if t := e.Totals; t != nil {
		out.Totals = psJSONUsage{
			Requests:   t.Requests,
			Errors:     t.Errors,
			LimitHits:  t.LimitHits,
			Input:      t.Input,
			Output:     t.Output,
			CacheRead:  t.CacheRead,
			CacheWrite: t.CacheWrite,
			CostUSD:    t.CostUSD,
		}
	}
	for _, key := range psModelKeysByRecency(e) {
		m := e.Models[key]
		purposes := map[string]psUsage{}
		for name, p := range m.ByPurpose {
			if p == nil || p.Requests <= 0 {
				continue
			}
			purposes[name] = psUsage{Requests: p.Requests, Input: p.Input, Output: p.Output, CostUSD: p.CostUSD}
		}
		out.Models = append(out.Models, psJSONModel{
			Provider:   m.Provider,
			Model:      m.Model,
			Requests:   m.Requests,
			Errors:     m.Errors,
			LimitHits:  m.LimitHits,
			Input:      m.Input,
			Output:     m.Output,
			CacheRead:  m.CacheRead,
			CacheWrite: m.CacheWrite,
			CostUSD:    m.CostUSD,
			FirstAt:    m.FirstAt,
			LastAt:     m.LastAt,
			LastError:  m.LastError,
			Roles:      m.Roles,
			Sources:    m.Sources,
			Purposes:   purposes,
		})
	}
	for _, a := range e.Agents {
		if a == nil {
			continue
		}
		out.Agents = append(out.Agents, psJSONAgent{
			ID: a.ID, Model: a.Model, Role: a.Role, Requests: a.Requests, LastAt: a.LastAt,
		})
	}
	return out
}
