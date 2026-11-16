package hub

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
)

func TestScopeTableExhaustive(t *testing.T) {
	isolateAPIGlobals(t)
	// The protocol constants moved to the wire leaf package: the former
	// protocol.go is now wire/protocol.go plus the live-work section in
	// wire/snapshots.go.
	var decls []ast.Decl
	for _, name := range []string{"protocol.go", "snapshots.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "wire", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		decls = append(decls, f.Decls...)
	}
	known := map[string]bool{}
	for _, decl := range decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			v := spec.(*ast.ValueSpec)
			for _, name := range v.Names {
				if len(name.Name) >= 3 && name.Name[:3] == "Cmd" {
					for _, value := range v.Values {
						if lit, ok := value.(*ast.BasicLit); ok {
							known[strings.Trim(lit.Value, "\"")] = true
						}
					}
				}
			}
		}
	}
	for cmd := range known {
		if _, ok := ScopeFor(cmd); !ok {
			t.Errorf("missing scope entry %q", cmd)
		}
	}
	for cmd := range commandScopes {
		if !known[cmd] {
			t.Errorf("stale scope entry %q", cmd)
		}
	}
}

func TestAuthorizeCommandScopeMatrix(t *testing.T) {
	isolateAPIGlobals(t)
	cases := []struct {
		name       string
		principal  apiauth.Principal
		command    string
		allowAdmin bool
		want       error
	}{
		{"read/read", apiauth.Principal{Scopes: []string{"read"}}, "list_sessions", false, nil},
		{"read/run", apiauth.Principal{Scopes: []string{"read"}}, "send_message", false, ErrScopeForbidden},
		{"read/config", apiauth.Principal{Scopes: []string{"read"}}, "set_system_prompt", false, ErrScopeForbidden},
		{"read/admin", apiauth.Principal{Scopes: []string{"read"}}, "set_provider_key", true, ErrScopeForbidden},
		{"run/read", apiauth.Principal{Scopes: []string{"run"}}, "list_sessions", false, nil},
		{"run/run", apiauth.Principal{Scopes: []string{"run"}}, "send_message", false, nil},
		{"run/config", apiauth.Principal{Scopes: []string{"run"}}, "set_system_prompt", false, ErrScopeForbidden},
		{"run/admin", apiauth.Principal{Scopes: []string{"run"}}, "set_provider_key", true, ErrScopeForbidden},
		{"config/read", apiauth.Principal{Scopes: []string{"config"}}, "list_sessions", false, nil},
		{"config/run", apiauth.Principal{Scopes: []string{"config"}}, "send_message", false, nil},
		{"config/config", apiauth.Principal{Scopes: []string{"config"}}, "set_system_prompt", false, nil},
		{"config/admin", apiauth.Principal{Scopes: []string{"config"}}, "set_provider_key", true, ErrScopeForbidden},
		{"admin/read", apiauth.Principal{Scopes: []string{"admin"}}, "list_sessions", false, nil},
		{"admin/run", apiauth.Principal{Scopes: []string{"admin"}}, "send_message", false, nil},
		{"admin/config", apiauth.Principal{Scopes: []string{"admin"}}, "set_system_prompt", false, nil},
		{"admin/admin", apiauth.Principal{Scopes: []string{"admin"}}, "set_provider_key", true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AuthorizeCommand(tc.principal, tc.command, CommandPolicy{AllowAdmin: tc.allowAdmin})
			if !errors.Is(err, tc.want) || (err == nil) != (tc.want == nil) {
				t.Fatalf("AuthorizeCommand error=%v, want %v", err, tc.want)
			}
		})
	}
	for _, p := range []apiauth.Principal{{}, {Scopes: []string{"unknown"}}} {
		for _, command := range []string{"list_sessions", "send_message", "set_system_prompt", "set_provider_key"} {
			want := error(ErrScopeForbidden)
			if command == "set_provider_key" {
				want = ErrScopeForbidden
			}
			if err := AuthorizeCommand(p, command, CommandPolicy{AllowAdmin: true}); !errors.Is(err, want) {
				t.Errorf("scopes %v command %s: error=%v want %v", p.Scopes, command, err, want)
			}
		}
	}
	admin := apiauth.Principal{Scopes: []string{"admin"}}
	if err := AuthorizeCommand(admin, "set_provider_key", CommandPolicy{}); !errors.Is(err, ErrAdminDisallowed) {
		t.Fatalf("admin without listener policy: %v", err)
	}
	if err := AuthorizeCommand(admin, "list_sessions", CommandPolicy{}); err != nil {
		t.Fatalf("admin denied non-admin command: %v", err)
	}
}

func TestAuthorizeCommandAdminPolicy(t *testing.T) {
	isolateAPIGlobals(t)
	p := apiauth.Principal{Scopes: []string{"admin"}}
	if !errors.Is(AuthorizeCommand(p, "shutdown_server", CommandPolicy{}), ErrAdminDisallowed) {
		t.Fatal("admin command did not return ErrAdminDisallowed")
	}
	if !errors.Is(AuthorizeCommand(p, "unknown", CommandPolicy{AllowAdmin: true}), ErrCommandUnmapped) {
		t.Fatal("unknown command did not return ErrCommandUnmapped")
	}
	if AuthorizeCommand(p, "shutdown_server", CommandPolicy{AllowAdmin: true}) != nil {
		t.Fatal("admin command denied")
	}
	if !errors.Is(AuthorizeCommand(apiauth.Principal{Scopes: []string{"read"}}, "unknown", CommandPolicy{AllowAdmin: true}), ErrCommandUnmapped) {
		t.Fatal("unmapped command did not return ErrCommandUnmapped")
	}
}

// isolateAPIGlobals local copy (sprint 0 L4): this test file moved from
// package server to hub; the helper it used lived in the staying server-side
// serve fixtures (ws_serve_slots_test.go:169, itself an S2/L2 verbatim copy
// of apiauth_chain_test.go:29-36 at HEAD). Verbatim copy again; servertest
// (L6/L7) absorbs it.
func isolateAPIGlobals(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dir)
	t.Setenv("RUSH_GLOBAL_CONFIG", dir)
	t.Setenv("RUSH_GLOBAL_DIR", dir)
}
