// Package heartbeat: snapshot readers, aggregation, and pruning.
package heartbeat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReadAll loads every parseable snapshot, retrying torn files once.
func ReadAll() ([]Entry, error) {
	dir := resolveDir()
	if dir == "" {
		return []Entry{}, nil
	}
	files, e := os.ReadDir(dir)
	if os.IsNotExist(e) {
		return []Entry{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []Entry{}
	for _, f := range files {
		if f.IsDir() || strings.HasPrefix(f.Name(), ".") || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		var en Entry
		for i := 0; i < 2; i++ {
			b, err := readFile(path)
			if err == nil {
				err = json.Unmarshal(b, &en)
			}
			if err == nil {
				break
			}
			if i == 0 {
				time.Sleep(50 * time.Millisecond)
			} else {
				en.PID = 0
			}
		}
		if en.PID == 0 {
			continue
		}
		en.File = f.Name()
		en.BeatAgeSec = int64(now().Sub(parseTime(en.LastBeat)).Seconds())
		var stale bool
		en.Alive, stale = processStatus(en.PID, en.ProcStart)
		switch {
		case en.StoredState == "stopped":
			en.State = "stopped"
		case stale:
			en.State = "stale"
		case en.LastRequestAt != "" && now().Sub(parseTime(en.LastRequestAt)) <= 30*time.Second:
			en.State = "running"
		default:
			en.State = "idle"
		}
		out = append(out, en)
	}
	return out, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// ModelSummary aggregates one provider/model across snapshots.
type ModelSummary struct {
	Model                                      string
	Live, Entries                              int
	Requests, Errors, LimitHits, Input, Output int64
	CostUSD                                    float64
	LastAt                                     time.Time
}

// Summarize aggregates models across snapshots.
func Summarize(es []Entry) []ModelSummary {
	m := map[string]*ModelSummary{}
	for _, e := range es {
		for _, x := range e.Models {
			k := x.Provider + "/" + x.Model
			s := m[k]
			if s == nil {
				s = &ModelSummary{Model: k}
				m[k] = s
			}
			s.Entries++
			if e.Alive {
				s.Live++
			}
			s.Requests += x.Requests
			s.Errors += x.Errors
			s.LimitHits += x.LimitHits
			s.Input += x.Input
			s.Output += x.Output
			s.CostUSD += x.CostUSD
			if t := parseTime(x.LastAt); t.After(s.LastAt) {
				s.LastAt = t
			}
		}
	}
	out := make([]ModelSummary, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out
}

// ownTempName matches only OUR in-flight temp files, not arbitrary dot-tmp.
func ownTempName(n string) bool {
	return strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".json.tmp") && strings.Contains(n, "--")
}

// EndSession marks a root stopped, flushes it, then drops the in-memory row.
func EndSession(rootSessionID string) {
	id := rootID(Context{RootSessionID: rootSessionID})
	registry.Lock()
	r := registry.rows[id]
	if r == nil {
		registry.Unlock()
		return
	}
	r.State = "stopped"
	r.dirty = true
	r.generation++
	registry.Unlock()
	finished := make(chan struct{})
	flushFuncMu.RLock()
	f := flushNow
	flushFuncMu.RUnlock()
	go func() {
		f(true)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
	}
	registry.Lock()
	if registry.rows[id] == r {
		delete(registry.rows, id)
	}
	registry.Unlock()
}

// Prune removes old stopped/stale snapshots, own stale temps, and
// unparsable json older than the cutoff. Never removes alive-or-unknown.
func Prune(olderThan time.Duration) (int, error) {
	dir := resolveDir()
	if dir == "" {
		return 0, nil
	}
	files, e := os.ReadDir(dir)
	if os.IsNotExist(e) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	removed := 0
	cut := now().Add(-olderThan)
	for _, f := range files {
		path := filepath.Join(dir, f.Name())
		if f.IsDir() {
			continue
		}
		if ownTempName(f.Name()) {
			if i, err := f.Info(); err == nil && i.ModTime().Before(cut) && os.Remove(path) == nil {
				removed++
			}
			continue
		}
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		var en Entry
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &en) != nil {
			if i, se := f.Info(); se == nil && i.ModTime().Before(cut) && os.Remove(path) == nil {
				removed++
			}
			continue
		}
		if en.PID <= 0 {
			continue
		}
		alive, stale := processStatus(en.PID, en.ProcStart)
		if alive && !stale {
			continue
		}
		if !parseTime(en.LastBeat).Before(cut) {
			continue
		}
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed, nil
}
