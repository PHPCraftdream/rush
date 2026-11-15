package heartbeat

import "context"

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
