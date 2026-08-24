// Environment overlay and credential probes for provider setup: the
// CRUSH_-prefixed shadow map fed to variable resolution, and the AWS
// credential detection behind the Bedrock provider check.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/home"
)

// crushEnvOverlay scans e for "CRUSH_X" entries and returns a map of the
// bare "X" -> value they should shadow, so a config value written as
// "$FOO" resolves against CRUSH_FOO when the caller has set it (the
// documented escape hatch for overriding a variable crush itself doesn't
// own, e.g. in CI or a sandboxed agent run without touching the real
// FOO for every other process on the machine).
//
// This is a pure function of the given Env — it does not read or mutate
// os.Environ() directly, so it is safe to call concurrently from multiple
// ConfigStore reloads without any of them observing (or clobbering) each
// other's overlay. Historically (PushPopCrushEnv) this same computation
// was applied by temporarily os.Setenv-ing the process's real environment
// and restoring it after the resolver ran — that mutated global state
// visible to every other goroutine and child process for the duration
// (including a concurrent reload's own resolution, or a sub-agent
// subprocess spawned by unrelated code mid-resolve) and, because
// os.Getenv cannot distinguish "unset" from "set to empty", left a
// variable that was absent before the call permanently set to "" after
// "restoring" it. See internal/env.NewOverlay for the replacement
// mechanism: the overlay computed here is passed explicitly to the
// resolver and to configureProviders' own env.Get calls instead.
func crushEnvOverlay(e env.Env) map[string]string {
	const prefix = "CRUSH_"
	overlay := make(map[string]string)
	for _, ev := range e.Env() {
		key, _, ok := strings.Cut(ev, "=")
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		bare := strings.TrimPrefix(key, prefix)
		overlay[bare] = e.Get(key)
	}
	return overlay
}

func hasAWSCredentials(env env.Env) bool {
	if env.Get("AWS_BEARER_TOKEN_BEDROCK") != "" {
		return true
	}

	if env.Get("AWS_ACCESS_KEY_ID") != "" && env.Get("AWS_SECRET_ACCESS_KEY") != "" {
		return true
	}

	if env.Get("AWS_PROFILE") != "" || env.Get("AWS_DEFAULT_PROFILE") != "" {
		return true
	}

	if env.Get("AWS_REGION") != "" || env.Get("AWS_DEFAULT_REGION") != "" {
		return true
	}

	if env.Get("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" ||
		env.Get("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return true
	}

	if _, err := os.Stat(filepath.Join(home.Dir(), ".aws/credentials")); err == nil && !testing.Testing() {
		return true
	}
	if _, err := os.Stat(filepath.Join(home.Dir(), ".aws/login")); err == nil && !testing.Testing() {
		return true
	}

	return false
}
