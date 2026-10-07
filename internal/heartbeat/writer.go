// Package heartbeat: background worker, flush, and shutdown.
package heartbeat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/rush/internal/procinfo"
)

func worker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastFlush := time.Time{}
	lastBeat := now()
	for range ticker.C {
		registry.Lock()
		dirty := false
		for _, r := range registry.rows {
			if r.dirty {
				dirty = true
				break
			}
		}
		beat := !lastBeat.IsZero() && now().Sub(lastBeat) >= 30*time.Second
		registry.Unlock()
		if dirty && (lastFlush.IsZero() || now().Sub(lastFlush) >= 5*time.Second) {
			requestFlush(false)
			lastFlush = now()
		}
		if beat {
			touchActive()
			lastBeat = now()
		}
	}
}

func touchActive() {
	registry.Lock()
	for _, r := range registry.rows {
		if !r.dirty && r.State == "running" && r.LastRequestAt != "" && now().Sub(parseTime(r.LastRequestAt)) <= time.Hour {
			r.LastBeat = now().UTC().Format(time.RFC3339)
			r.dirty = true
			r.generation++
		}
	}
	registry.Unlock()
	requestFlush(false)
}

func cloneAgent(a *agent) *agent {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}

func flush(force bool) {
	flushMu.Lock()
	defer flushMu.Unlock()
	lastFlushMu.Lock()
	throttled := !lastFlushAt.IsZero() && now().Sub(lastFlushAt) < 5*time.Second
	lastFlushMu.Unlock()
	dir := resolveDir()
	if dir == "" || throttled && !force {
		return
	}
	registry.Lock()
	rows := []*record{}
	for _, r := range registry.rows {
		if r.dirty {
			r.LastBeat = now().UTC().Format(time.RFC3339)
			c := *r
			c.Totals = cloneModel(r.Totals)
			c.Models = cloneModels(r.Models)
			c.Agents = cloneAgents(r.Agents)
			c.SlotsAtStart = copySlots(r.SlotsAtStart)
			rows = append(rows, &c)
		}
	}
	registry.Unlock()
	for _, r := range rows {
		b, e := json.Marshal(r)
		if e != nil {
			continue
		}
		if len(b) > snapshotBudget {
			b = compactSnapshot(r)
		}
		path := filepath.Join(dir, filename(launchCwd, r.identity))
		tmp := filepath.Join(dir, "."+filepath.Base(path)+".json.tmp")
		if os.MkdirAll(dir, 0o700) != nil {
			continue
		}
		if writeFile(tmp, b, 0o600) != nil {
			continue
		}
		if renameFile(tmp, path) != nil {
			continue
		}
		lastFlushMu.Lock()
		lastFlushAt = now()
		lastFlushMu.Unlock()
		registry.Lock()
		if current := registry.rows[r.identity]; current != nil && current.generation == r.generation {
			current.dirty = false
		}
		registry.Unlock()
	}
}

const snapshotBudget = 16 * 1024

// Shutdown marks rows stopped and publishes final state within timeout.
// Every caller runs its own bounded wait; flush is serialised by flushMu.
func Shutdown(timeout time.Duration) {
	if timeout < 0 {
		timeout = 0
	}
	registry.Lock()
	for _, r := range registry.rows {
		r.State = "stopped"
		r.dirty = true
		r.generation++
	}
	registry.Unlock()
	done := make(chan struct{})
	go func() {
		requestFlush(true)
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// Entry is a decoded snapshot plus live status.
type Entry struct {
	File          string            `json:"-"`
	Alive         bool              `json:"-"`
	State         string            `json:"-"`
	BeatAgeSec    int64             `json:"-"`
	Version       int               `json:"v"`
	PID           int               `json:"pid"`
	PPID          int               `json:"ppid"`
	Parent        string            `json:"parent"`
	ProcStart     string            `json:"proc_start"`
	LaunchCwd     string            `json:"launch_cwd"`
	Workspace     string            `json:"workspace"`
	Session       string            `json:"session"`
	StoredState   string            `json:"state"`
	StartedAt     string            `json:"started_at"`
	LastBeat      string            `json:"last_beat"`
	LastRequestAt string            `json:"last_request_at"`
	SlotsAtStart  map[string]string `json:"slots_at_start"`
	Totals        *model            `json:"totals"`
	Models        map[string]*model `json:"models"`
	Agents        []*agent          `json:"agents"`
	// RateLimitedUntil is the RFC3339 deadline of a pending provider
	// rate-limit wait; empty when not waiting.
	RateLimitedUntil string `json:"rate_limited_until,omitempty"`
}

// probeFn indirection lets tests stub the tri-state probe.
var probeFn = procinfo.Probe

// processStatus tri-states liveness: dead/reused => stale, unknown => alive.
func processStatus(pid int, expected string) (alive, stale bool) {
	a, token, known := probeFn(pid)
	if !a {
		return false, true
	}
	if !known {
		return true, false
	}
	if expected != "" && token != "" && token != expected {
		return true, true
	}
	return true, false
}

func processAlive(pid int, expected string) bool {
	alive, stale := processStatus(pid, expected)
	return alive && !stale
}
