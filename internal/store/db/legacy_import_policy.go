package db

// LegacyTablePolicy explicitly classifies legacy and SQLite-owned tables.
type LegacyTablePolicy struct {
	Name   string
	Carry  bool
	Reason string
}

// LegacyImportPolicy documents every table created by the migration history,
// plus Goose and SQLite-owned tables encountered during discovery.
var LegacyImportPolicy = []LegacyTablePolicy{
	{Name: "sessions", Carry: true, Reason: "durable session identity, settings, budgets, todos, and history ownership; clear shutdown cancellation"},
	{Name: "messages", Carry: true, Reason: "conversation history, including unfinished assistant transcript rows"},
	{Name: "files", Carry: true, Reason: "durable versioned file history"},
	{Name: "read_files", Carry: true, Reason: "durable read-before-write tracking"},
	{Name: "session_permissions", Carry: true, Reason: "durable user-granted permission settings"},
	{Name: "queue_tasks", Carry: true, Reason: "independent operator CLI queue; reset in-flight running task to pending for safe retry"},
	{Name: "pending_injects", Carry: true, Reason: "accepted user input transport; FK ties it to preserved session/message"},
	{Name: "session_run_queue", Carry: true, Reason: "durable continuation queue; release expired/foreign leases for recovery"},
	{Name: "orphan_call_outbox", Carry: true, Reason: "durable queued session calls the run queue refused; same data class as the carried session_run_queue; reset dead 'processing' claims to pending so the drain can enqueue them"},
	{Name: "async_hosts", Carry: false, Reason: "diagnostic host registry from the retired runtime; old host locks are probed before import"},
	{Name: "session_drivers", Carry: false, Reason: "runtime ownership markers cannot remain authoritative in a new database generation"},
	{Name: "session_notices", Carry: true, Reason: "durable supervision/question/wake transport; void old job-bound notices and preserve their text as history"},
	{Name: "wake_schedules", Carry: true, Reason: "durable user schedules; release runtime lease owner/expiry"},
	{Name: "async_jobs", Carry: false, Reason: "old execution authority; unfinished work must never be resumed/replayed"},
	{Name: "batch_nodes", Carry: false, Reason: "preparation-era batch projection is explicitly abandoned"},
	{Name: "batch_control_requests", Carry: false, Reason: "preparation-era control intent is explicitly abandoned"},
	{Name: "batch_runner_checkpoints", Carry: false, Reason: "preparation-era runner lifecycle state is explicitly abandoned"},
	{Name: "batch_timeout_checkins", Carry: false, Reason: "preparation-era check-in receipts are explicitly abandoned"},
	{Name: "goose_db_version", Carry: false, Reason: "engine-managed migration metadata; initialize fresh lineage only"},
	{Name: "sqlite_sequence", Carry: false, Reason: "SQLite-managed AUTOINCREMENT state, rebuilt by carried inserts; the legacy seq is not carried, so notice ids deleted by retention may be reissued (nothing durable references notice ids)"},
}

func legacyImportPolicy(name string) (LegacyTablePolicy, bool) {
	for _, p := range LegacyImportPolicy {
		if p.Name == name {
			return p, true
		}
	}
	return LegacyTablePolicy{}, false
}
