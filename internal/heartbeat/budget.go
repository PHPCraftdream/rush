package heartbeat

import "encoding/json"

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
