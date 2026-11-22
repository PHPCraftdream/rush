// Canonical command/skill source assets for the admin/install family.
//
// The //go:embed declaration lives here (the .md/.yaml asset files live in
// this directory), but the conversion implementation that consumes them moved
// to the targets subpackage to break the install <-> targets import cycle.
// init() injects the embedded FS into targets exactly once; the targets
// package's white-box tests may instead inject their own fs.FS over the same
// byte-identical assets.
package install

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/admin/install/targets"
)

//go:embed *.md *.yaml
var skillSourceFiles embed.FS

func init() {
	targets.InjectSkillSources(skillSourceFiles, claudeWrushCommandTemplate, claudeWcrushCommandTemplate)
}

func validateClaudeInitSources(sourceFS fs.FS) error {
	if _, _, err := targets.LoadSkillSourceFrom(sourceFS, "claude_slash_command", targets.SkillTargetClaude); err != nil {
		return fmt.Errorf("rush slash-command source: %w", err)
	}
	if _, _, err := targets.LoadSkillSourceFrom(sourceFS, "claude_crush_fallback_command", targets.SkillTargetClaude); err != nil {
		return fmt.Errorf("rush-fallback slash-command source: %w", err)
	}
	return nil
}
