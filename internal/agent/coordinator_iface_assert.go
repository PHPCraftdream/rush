package agent

// Compile-time wiring guards for the OPTIONAL interfaces *coordinator exposes.
// Their consumers type-assert on the Coordinator value (internal/app, cmd and
// server: `AgentCoordinator.(agent.ReactionDebtSource)`, `.(agent.AutoTurnHolder)`,
// `.(agent.ParkedSubAgentWorkReporter)`) so a signature drift still compiles
// and silently turns the feature off -- two earlier merges did exactly that.
// The assertions live in the package that owns the concrete type, where a
// drift fails the build.
var (
	_ AutoTurnHolder             = (*coordinator)(nil)
	_ ReactionDebtSource         = (*coordinator)(nil)
	_ ParkedSubAgentWorkReporter = (*coordinator)(nil)
)
