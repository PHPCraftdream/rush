// X1 residual-fix helpers (batch shadow, 2026-10-19): fixture-side
// glue shared by the drain-gate/launch-gate residuals. Methods on
// attemptFixture only; shared port helpers stay untouched.
package agent

// ensureDrainDriver re-registers the fixture session's delegation driver if
// the coordinator's registry no longer holds it.
//
// Why this exists: a fixture whose debt was seeded
// through seedFixtureJobDebt has NO armed workLedger job (the ledger Start
// registration is legacy-only bookkeeping the batch path never performs -- see
// wakeDebtFixture.claimAndFinish's batch branch). A release-driven recheckChild
// then reads childScopeDrained == true with nothing armed and tears the
// driver down (work_ledger_delegation.go: releaseDriverIfScopeClosed via
// recheckChild's "nothing left armed" branch), while the legacy path kept it
// because l.running still holds the claim. A later wake then takes
// drainCallFor's ROOT branch, which dereferences the bare fixture's nil
// c.cfg (resolveSessionModels -> config.(*ConfigStore).Snapshot) and panics.
// Re-registering restores exact parity: every wake of this fixture takes
// the driver branch throughout the test.
func (f *attemptFixture) ensureDrainDriver() {
	f.t.Helper()
	if _, ok := f.coord.subAgentDrivers.get(f.sessID); ok {
		return
	}
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: f.sessID}})
}
