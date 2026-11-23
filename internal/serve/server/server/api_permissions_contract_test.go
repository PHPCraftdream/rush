package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIPermissionProductionFilesDoNotChangeSessionApprovalPolicy(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "api_") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"GrantPersistent", "AutoApproveSession"} {
			if strings.Contains(string(content), forbidden) {
				t.Errorf("%s mentions forbidden permission operation %s", name, forbidden)
			}
		}
	}
}

func TestAPIPermissionAnswerBadRequestsHaveExactProblemBodies(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"unknown-field", `{"decision":"allow","extra":true}`},
		{"unsupported-decision", `{"decision":"allow_session"}`},
		{"malformed-json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := permissionHandlerRequest(t, NewAPIHandler(apiPermissionRoutes(nil)), "POST", "/api/v1/permissions/p1", tc.body, Principal{TokenID: "owner", Scopes: []string{"run"}})
			want := "{\"type\":\"about:blank\",\"title\":\"Bad Request\",\"status\":400,\"detail\":\"invalid permission decision\"}\n"
			if got.Code != 400 || got.Body.String() != want {
				t.Fatalf("response = %d %q, want 400 %q", got.Code, got.Body.String(), want)
			}
		})
	}
}
