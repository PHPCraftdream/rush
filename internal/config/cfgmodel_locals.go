package config

// D2-1 boundary closures: byte-identical local copies of small unexported
// pure helpers and constants that staying-root files still need after the
// model declarations moved to internal/config/model/cfgmodel. Each moved
// name has exactly one moved definition; these are separate closures
// documented in the D2-1 lane report (11 names, within the ≤15/cluster
// budget). settingsAuditSource/liftsSettingsLock mirror the cfgmodel
// settings_authority.go helpers: liftsSettingsLock cannot be a method in
// this package (the aliased type is non-local), so it closes as a plain
// function and settings_lock.go:338 was adjusted accordingly.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
)

// settingsKeyFirstComponent extracts the first sjson path component.
// appName and defaultDataDirectory are byte-identical local copies of the
// small unexported constants that moved to cfgmodel (model/config.go:41-42);
// staying-root datadir.go and globalpaths.go still read them (closure 12 of
// the <=15 budget).
const (
	appName              = "rush"
	defaultDataDirectory = ".rush"
)

func settingsKeyFirstComponent(key string) string {
	first := strings.TrimPrefix(key, ":")
	var component strings.Builder
	for i := 0; i < len(first); i++ {
		switch first[i] {
		case '.':
			return component.String()
		case '\\':
			i++
			if i < len(first) {
				component.WriteByte(first[i])
			}
		default:
			component.WriteByte(first[i])
		}
	}
	return component.String()
}

// canonicalModelsJSON canonicalizes a models value for hashing. Absent, null,
// or empty values are treated as not present.
func canonicalModelsJSON(raw string) (canonical string, present bool, err error) {
	value := gjson.Parse(raw)
	if !value.Exists() || value.Type == gjson.Null {
		return "", false, nil
	}
	if value.IsObject() && len(value.Map()) == 0 {
		return "", false, nil
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return "", false, err
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

// modelsIntegritySignature hashes the canonicalized models value.
// Constant mixed into the cache revision hash; the name is deliberately generic.
const hashSalt = "7c1f4e9a2b8d6350af02c4e8b19d5736"

func modelsIntegritySignature(canonical string) string {
	digest := sha256.Sum256([]byte("v1|" + hashSalt + "|" + canonical))
	return hex.EncodeToString(digest[:])
}

func validateResponsesTransportWrite(raw []byte, keys []string) error {
	for _, key := range keys {
		parts := strings.Split(key, ".")
		touched := key == "options" || key == "options.responses_transport" || strings.HasPrefix(key, "options.responses_transport.") || key == "providers" ||
			(len(parts) >= 2 && parts[0] == "providers" && (len(parts) == 2 || parts[2] == "responses_transport"))
		if !touched {
			continue
		}
		var document struct {
			Options struct {
				Transport ResponsesTransport `json:"responses_transport"`
			} `json:"options"`
			Providers map[string]struct {
				Transport ResponsesTransport `json:"responses_transport"`
			} `json:"providers"`
		}
		return json.Unmarshal(raw, &document)
	}
	return nil
}

// settingsAuditSource maps an authority to the SettingsEvent.Source label.
// The label is a fixed, secret-free token: it identifies the transport the
// write arrived on, never the person or their credentials.
func settingsAuditSource(authority SettingsWriteAuthority) string {
	if authority == SettingsAuthorityTrustedUI {
		return "ui"
	}
	return ""
}

// liftsSettingsLock reports whether this authority may bypass the
// password/settings-lock veto. Only the trusted-UI authority does.
func liftsSettingsLock(authority SettingsWriteAuthority) bool {
	return authority == SettingsAuthorityTrustedUI
}

// resolverInputFingerprint records the semantic input to the shell resolver.
// The resolver object itself is intentionally not compared: every reload gets
// a fresh process-environment reader and its identity is not semantic state.
func resolverInputFingerprint(values []string) [32]byte {
	copyValues := slices.Clone(values)
	slices.Sort(copyValues)
	hash := sha256.New()
	for _, value := range copyValues {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return sha256.Sum256(hash.Sum(nil))
}

// defaultInitializeAs and defaultContextPaths are byte-identical local
// copies of the moved cfgmodel declarations (model/config.go:43,46-64)
// still read by staying-root init.go and load_defaults.go (closures 13-14).
const defaultInitializeAs = "AGENTS.md"

var defaultContextPaths = []string{
	".github/copilot-instructions.md",
	".cursorrules",
	".cursor/rules/",
	"CLAUDE.md",
	"CLAUDE.local.md",
	"GEMINI.md",
	"gemini.md",
	"rush.md",
	"rush.local.md",
	"Rush.md",
	"Rush.local.md",
	"RUSH.md",
	"RUSH.local.md",
	"AGENTS.md",
	"agents.md",
	"Agents.md",
}

// configHasDynamicMCPResolution, mcpConfigHasDynamicResolution and
// hasCommandSubstitution are byte-identical local copies of the moved
// cfgmodel helpers (model/config_mcp.go:98,120,131) still called by
// staying-root init.go, load.go and store.go (closures 15).
func configHasDynamicMCPResolution(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, m := range cfg.MCP {
		if mcpConfigHasDynamicResolution(m) {
			return true
		}
	}
	return false
}

func mcpConfigHasDynamicResolution(m MCPConfig) bool {
	if hasCommandSubstitution(m.Command) || hasCommandSubstitution(m.URL) {
		return true
	}
	for _, value := range m.Env {
		if hasCommandSubstitution(value) {
			return true
		}
	}
	for _, value := range m.Args {
		if hasCommandSubstitution(value) {
			return true
		}
	}
	for _, value := range m.Headers {
		if hasCommandSubstitution(value) {
			return true
		}
	}
	return false
}

func hasCommandSubstitution(value string) bool {
	return shell.HasCommandSubstitution(value)
}

// maxRecentModelsPerType is a byte-identical local copy of the moved
// cfgmodel constant (model/config.go:458) still read by staying-root
// store_models.go (closure 16).
const maxRecentModelsPerType = 5

// allToolNames is a byte-identical local copy of the moved cfgmodel helper
// (model/config.go:460) still used by staying-root tests (closure 17).
func allToolNames() []string {
	names := []string{
		"agent",
		"read_delegation_transcript",
		"ask_question",
		"bash",
		"git_read",
		"run_command",
		"rush_info",
		"rush_logs",
		"inspect_agent",
		"inject_agent",
		"stop_agent",
		"await_tasks",
		"wakein",
		"wakeon",
		"loop",
		"wake_list",
		"wake_cancel",
		"download",
		"edit",
		"multiedit",
		"fetch",
		"agentic_fetch",
		"glob",
		"grep",
		"ls",
		"sourcegraph",
		"todos",
		"view",
		"write",
		"list_mcp_resources",
		"read_mcp_resource",
		"fs_list",
		"fs_find",
		"fs_grep",
		"fs_read",
		"fs_write",
		"fs_replace",
		"fs_write_lines",
		"fs_delete",
	}
	// R12-H4: the batch runtime is unconditional, so the native batch
	// controls are always part of the inventory; the deleted job_kill and
	// job_output tools are gone from the list.
	names = append(names, "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop")
	return names
}

// providerRequestTimeout is a byte-identical local copy of the moved
// cfgmodel constant (model/network.go:59) still used by staying-root tests
// (closure 18).
const providerRequestTimeout = 10 * time.Second

// sanitizeResolveError is a byte-identical local copy of the moved
// envresolve helper still used by staying-root tests (closure 19).
func sanitizeResolveError(template string, err error) error {
	if err == nil {
		return nil
	}
	return &resolveError{
		template: template,
		msg:      scrubErrorMessage(err.Error()),
		inner:    err,
	}
}

// resolveError (+Error/Unwrap) is a byte-identical local copy of the moved
// envresolve type (model/envresolve/resolve.go:254-268) required by the
// sanitizeResolveError closure (closure 19, continued).
// resolveError is the concrete type returned by sanitizeResolveError.
// Its Error() method returns the template + scrubbed inner message;
// Unwrap exposes the original error so errors.Is/As continue to work.
type resolveError struct {
	template string
	msg      string
	inner    error
}

func (e *resolveError) Error() string {
	return fmt.Sprintf("resolving %q: %s", e.template, e.msg)
}

func (e *resolveError) Unwrap() error { return e.inner }

// scrubErrorMessage is a byte-identical local copy of the moved envresolve
// helper required by the sanitizeResolveError closure (closure 19, continued).
func scrubErrorMessage(s string) string {
	if len(s) > maxResolveErrBytes {
		s = s[:maxResolveErrBytes]
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' || c == '\n' || (c >= 0x20 && c < 0x7f) {
			out[i] = c
			continue
		}
		out[i] = '?'
	}
	return string(out)
}

// maxResolveErrBytes is a byte-identical local copy of the moved envresolve
// constant required by the scrubErrorMessage closure (closure 19, continued).
const maxResolveErrBytes = 512
