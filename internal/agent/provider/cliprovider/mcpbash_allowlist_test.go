package cliprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The restricted-run gate routes any tool whose permission params implement
// RunAllowlistCommand through allow_bash-pattern scrutiny (see
// internal/permission/runallowlist.go). These assertions pin that
// mcpBashInput keeps satisfying that contract so cliprovider bash requests
// (ToolName "bash", Params mcpBashInput) retain command-level scrutiny.
// Behavioral gating for this type is covered by the app-level and
// permission-level tests using stand-ins with the same contract.
var _ interface{ RunAllowlistCommand() string } = mcpBashInput{}

func TestMCPBashInput_RunAllowlistCommand(t *testing.T) {
	input := mcpBashInput{Command: "go test ./...", Description: "run tests"}
	assert.Equal(t, "go test ./...", input.RunAllowlistCommand())
	assert.Equal(t, "", mcpBashInput{}.RunAllowlistCommand())
}
