package heartbeat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// cloneModel deep-copies a model entry.
func cloneModel(m *model) *model {
	if m == nil {
		return nil
	}
	c := *m
	c.Roles = append([]string(nil), m.Roles...)
	c.Sources = append([]string(nil), m.Sources...)
	c.ByPurpose = make(map[string]*purposeStats, len(m.ByPurpose))
	for k, v := range m.ByPurpose {
		if v == nil {
			continue
		}
		p := *v
		c.ByPurpose[k] = &p
	}
	return &c
}

// cloneModels deep-copies the model map.
func cloneModels(in map[string]*model) map[string]*model {
	out := make(map[string]*model, len(in))
	for k, v := range in {
		out[k] = cloneModel(v)
	}
	return out
}

// cloneAgents shallow-copies the agent slice.
func cloneAgents(in []*agent) []*agent {
	out := make([]*agent, len(in))
	for i, a := range in {
		out[i] = cloneAgent(a)
	}
	return out
}

// compactSnapshot shrinks a copy of r under the snapshot budget.
// Order: drop agent details, then per-purpose maps, then fold models into
// "(other)". Totals always survive; the record is always marshalable.
func compactSnapshot(r *record) []byte {
	c := *r
	c.Models = cloneModels(r.Models)
	c.Totals = cloneModel(r.Totals)
	c.Agents = cloneAgents(r.Agents)
	c.Workspace = boundedRunes(c.Workspace, 64)
	c.LaunchCwd = boundedRunes(c.LaunchCwd, 96)
	c.SlotsAtStart = copySlots(c.SlotsAtStart)
	b := marshalRecord(&c)
	for len(b) > snapshotBudget && len(c.Agents) > 0 {
		c.Agents = c.Agents[:len(c.Agents)-1]
		b = marshalRecord(&c)
	}
	for len(b) > snapshotBudget && shrinkPurposeMaps(&c) {
		b = marshalRecord(&c)
	}
	for len(b) > snapshotBudget && foldSnapshotModels(&c) {
		b = marshalRecord(&c)
	}
	// hardTrim guarantees the budget: totals counters always survive.
	if len(b) > snapshotBudget {
		hardTrim(&c)
		b = marshalRecord(&c)
	}
	return b
}

// hardTrim drops every non-counter field to fit the budget.
func hardTrim(r *record) {
	r.Agents = nil
	r.Workspace = ""
	r.LaunchCwd = ""
	r.SlotsAtStart = map[string]string{}
	shrink := func(m *model) {
		if m == nil {
			return
		}
		m.ByPurpose = map[string]*purposeStats{}
		m.Roles = nil
		m.Sources = nil
	}
	shrink(r.Totals)
	for _, m := range r.Models {
		shrink(m)
	}
	r.Session = boundedRunes(r.Session, 64)
}

func marshalRecord(r *record) []byte {
	b, _ := json.Marshal(r)
	return b
}

// shrinkPurposeMaps drops one purpose bucket per call.
func shrinkPurposeMaps(r *record) bool {
	changed := false
	shrink := func(m *model) {
		if m != nil && len(m.ByPurpose) > 0 {
			for k := range m.ByPurpose {
				delete(m.ByPurpose, k)
				changed = true
				break
			}
		}
	}
	shrink(r.Totals)
	for _, m := range r.Models {
		shrink(m)
	}
	return changed
}

// foldSnapshotModels folds everything into a single "(other)" entry.
// Returns true only on an actual reduction of the model count.
func foldSnapshotModels(r *record) bool {
	if len(r.Models) <= 1 {
		return false
	}
	m := &model{Model: otherKey, ByPurpose: map[string]*purposeStats{}}
	for _, old := range r.Models {
		foldModel(m, old)
	}
	r.Models = map[string]*model{otherKey: m}
	return true
}

// boundList trims and bounds a string list.
func boundList(in []string, n, sz int) []string {
	if len(in) > n {
		in = in[:n]
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = boundedRunes(s, sz)
	}
	return out
}

// RecordFailure counts one failed provider call on the model and totals
// WITHOUT incrementing Requests. Unknown models create their entry.
// Redaction and the 200-rune bound are applied here, as everywhere.
func RecordFailure(ctx context.Context, provider, modelName string, err error) {
	if err == nil {
		return
	}
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
	m.Errors++
	r.Totals.Errors++
	m.LastError = RedactError(err)
	if isLimit(err.Error()) {
		m.LimitHits++
		r.Totals.LimitHits++
	}
	r.dirty = true
	r.generation++
}

// DefaultDir is the global heartbeat directory: RUSH_HEARTBEAT_DIR wins,
// else heartbeat/ next to the global settings file — derived from
// workspaceDir, the caller's global workspace directory (a func so it is
// resolved only when the old code resolved it). An active
// --global-dir root (globalDirActive) pins the directory to the workspace,
// so RUSH_HEARTBEAT_DIR cannot override it. A test binary without an
// isolated global dir returns "" so tests never write the real registry.
func DefaultDir(workspaceDir func() string, globalDirActive bool) string {
	// An isolated global dir (RUSH_GLOBAL_DIR) does not lift the test guard
	// below: the process-wide writer follows the environment of whichever test
	// runs now and would keep writing into that test's TempDir while it is
	// being removed.
	if globalDirActive && !skipDefaultDirInTests {
		return filepath.Join(workspaceDir(), "heartbeat")
	}
	if d := os.Getenv("RUSH_HEARTBEAT_DIR"); d != "" {
		return d
	}
	// A test binary writes snapshots only into an explicit RUSH_HEARTBEAT_DIR:
	// the process-wide writer outlives each test and would race its TempDir.
	if skipDefaultDirInTests {
		return ""
	}
	return filepath.Join(workspaceDir(), "heartbeat")
}

// skipDefaultDirInTests is true in a test binary; the dir test turns it off.
var skipDefaultDirInTests = testing.Testing()
