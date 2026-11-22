package settingsctl

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
)

// TestTokenBindingJSONPresentation is the white-box unit half of the binding
// presentation coverage; the CLI subprocess path stays in package cmd's
// tokens_binding_test.go (true rush binary exit-code coverage).
func TestTokenBindingJSONPresentation(t *testing.T) {
	binding := config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: "sess", Workspace: filepath.Clean("/tmp/work")}}
	got := tokenBindingPresentation(binding)
	if !strings.Contains(got, "session=sess") || !strings.Contains(got, "workspace="+filepath.Clean("/tmp/work")) {
		t.Fatalf("binding presentation = %q", got)
	}
}
