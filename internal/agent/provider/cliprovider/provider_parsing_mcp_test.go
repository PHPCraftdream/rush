package cliprovider

// CL-A: moved out of clipspec/provider_parsing_test.go by main subject — it
// white-box-tests the facade's MCP server constructor (newRushMCPServer),
// which stays behind (CL-B moves it to clipmcp). Test body unchanged.

import (
	"context"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/clipspec"
)

// TestCodexMCPConfigArgs_UsesRealServerToken exercises CodexMCPConfigArgs
// against a genuinely running rushMCPServer (not a hand-written literal
// like the test above), confirming the fix holds against the actual random
// token newRushMCPServer generates, not just a placeholder addr string.
func TestCodexMCPConfigArgs_UsesRealServerToken(t *testing.T) {
	srv, err := newRushMCPServer(context.Background(), nil, nil, "", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("newRushMCPServer: %v", err)
	}
	t.Cleanup(srv.Stop)

	args := clipspec.CodexMCPConfigArgs(srv.Addr())
	joined := strings.Join(args, " ")

	if strings.Contains(joined, srv.Token()) {
		t.Errorf("CodexMCPConfigArgs must never embed the real MCP server's token in its returned args, got: %v", args)
	}
	if strings.Contains(joined, "?token=") {
		t.Errorf("CodexMCPConfigArgs must not produce a query-param token, got: %v", args)
	}
	if !strings.Contains(joined, "mcp_servers.rush.bearer_token_env_var="+clipspec.CodexMCPTokenEnvVar) {
		t.Errorf("CodexMCPConfigArgs must reference %s as the bearer_token_env_var, got: %v", clipspec.CodexMCPTokenEnvVar, args)
	}
}
