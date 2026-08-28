package app

import "testing"

// TestRestrictMCPToCLI_SetsFlag pins the Option's own effect in isolation
// from mcp.Initialize's filtering logic (covered separately by
// internal/agent/tools/mcp's TestInitialize_RestrictToCLIEnabled) and from
// New()'s full boot (heavy — DB, config, provider validation). This is the
// one-line wiring New() reads via o.restrictMCPToCLI before calling
// mcp.Initialize; a full New() test would only re-prove this same line.
func TestRestrictMCPToCLI_SetsFlag(t *testing.T) {
	var o newOptions
	RestrictMCPToCLI()(&o)
	if !o.restrictMCPToCLI {
		t.Fatal("RestrictMCPToCLI must set restrictMCPToCLI")
	}

	var unset newOptions
	if unset.restrictMCPToCLI {
		t.Fatal("zero-value newOptions must default restrictMCPToCLI to false")
	}
}
