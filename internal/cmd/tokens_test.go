package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestParseTokenExpiry(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	now := mustParseTime(t, "2026-11-01T00:00:00Z")
	got, err := parseTokenExpiry("30d", now)
	if err != nil || got == nil || !got.Equal(now.Add(30*24*60*60*1e9)) {
		t.Fatalf("30d expiry = %v, %v", got, err)
	}
	if _, err := parseTokenExpiry("2026-12-01T12:30:00+02:00", now); err != nil {
		t.Fatalf("RFC3339 expiry rejected: %v", err)
	}
	if _, err := parseTokenExpiry("forever", now); err == nil {
		t.Fatal("invalid expiry accepted")
	}
	if _, err := parseTokenExpiry("0s", now); err == nil {
		t.Fatal("non-future expiry accepted")
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var tokenTestInvocation atomic.Uint64

func tokenTestEnv(t *testing.T, base string) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global-dir"))
	for key, sub := range map[string]string{
		"RUSH_GLOBAL_DATA": "data", "RUSH_GLOBAL_CONFIG": "config",
		"XDG_CONFIG_HOME": "xdg-config", "XDG_DATA_HOME": "xdg-data",
	} {
		t.Setenv(key, filepath.Join(base, sub))
	}
}

func tokenTestCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	base := t.TempDir()
	tokenTestEnv(t, base)
	code, stdout, stderr, _ := tokenTestCommandAtWithError(t, base, args...)
	return code, stdout, stderr
}

func tokenTestCommandAt(t *testing.T, base string, args ...string) (int, string, string) {
	code, stdout, stderr, _ := tokenTestCommandAtWithError(t, base, args...)
	return code, stdout, stderr
}

func tokenTestCommandAtWithError(t *testing.T, base string, args ...string) (int, string, string, string) {
	t.Helper()
	cwd := filepath.Join(base, "cwd-"+strconv.FormatUint(tokenTestInvocation.Add(1), 10))
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	return tokenTestCommandAtWithErrorInDir(t, cwd, args...)
}

func tokenTestCommandAtWithErrorInDir(t *testing.T, cwd string, args ...string) (int, string, string, string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	before, readErr := os.ReadDir(cwd)
	if readErr != nil {
		t.Fatal(readErr)
	}
	oldOut, oldErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	oldJSON, oldName, oldPermission, oldExpires := tokensJSON, tokensName, tokensPermission, tokensExpires
	oldScopes := append([]string(nil), tokensScopes...)
	restoreFlags := saveTokenCommandFlags(tokensCmd)
	resetTokenCommandFlags(tokensCmd)
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs(args)
	defer func() {
		restoreFlags()
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
		tokensJSON = oldJSON
		tokensName, tokensPermission, tokensExpires = oldName, oldPermission, oldExpires
		tokensScopes = oldScopes
	}()
	err = rootCmd.Execute()
	errorText := ""
	if err != nil {
		errorText = err.Error()
	}
	code := 0
	if err != nil {
		var ok bool
		code, ok = tokensExitCode(args, err)
		if !ok {
			code = 1
		}
	}
	entries, readErr := os.ReadDir(cwd)
	if readErr != nil || len(entries) != len(before) {
		t.Fatalf("command changed cwd entries: before=%v after=%v err=%v", before, entries, readErr)
	}
	for i := range before {
		if entries[i].Name() != before[i].Name() {
			t.Fatalf("command changed cwd entries: before=%v after=%v", before, entries)
		}
	}
	return code, stdout.String(), stderr.String(), errorText
}

func saveTokenCommandFlags(cmd *cobra.Command) func() {
	type flagState struct {
		flag    *pflag.Flag
		value   string
		slice   []string
		changed bool
	}
	var states []flagState
	var visit func(*cobra.Command)
	visit = func(current *cobra.Command) {
		for _, flags := range []*pflag.FlagSet{current.Flags(), current.PersistentFlags()} {
			flags.VisitAll(func(flag *pflag.Flag) {
				state := flagState{flag: flag, value: flag.Value.String(), changed: flag.Changed}
				if value, ok := flag.Value.(pflag.SliceValue); ok {
					state.slice = append([]string(nil), value.GetSlice()...)
				}
				states = append(states, state)
			})
		}
		for _, child := range current.Commands() {
			visit(child)
		}
	}
	visit(cmd)
	return func() {
		for _, state := range states {
			if value, ok := state.flag.Value.(pflag.SliceValue); ok {
				_ = value.Replace(state.slice)
			} else {
				_ = state.flag.Value.Set(state.value)
			}
			state.flag.Changed = state.changed
		}
	}
}

func resetTokenCommandFlags(cmd *cobra.Command) {
	reset := func(flags *pflag.FlagSet) {
		flags.VisitAll(func(flag *pflag.Flag) {
			if flag.Name == "scope" && flag.Value.Type() == "stringSlice" {
				fresh := pflag.NewFlagSet("tokens-create-reset", pflag.ContinueOnError)
				fresh.StringSliceVar(&tokensScopes, "scope", []string{"run"}, flag.Usage)
				flag.Value = fresh.Lookup("scope").Value
			} else if value, ok := flag.Value.(pflag.SliceValue); ok {
				_ = value.Replace([]string{"run"})
			} else {
				_ = flag.Value.Set(flag.DefValue)
			}
			flag.Changed = false
		})
	}
	reset(cmd.Flags())
	reset(cmd.PersistentFlags())
	for _, child := range cmd.Commands() {
		resetTokenCommandFlags(child)
	}
}

func TestValidateTokenID(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	for _, id := range []string{"deadbeef", "00000000"} {
		if err := validateTokenID(id); err != nil {
			t.Fatalf("valid id %q rejected: %v", id, err)
		}
	}
	for _, id := range []string{"DEADBEEF", "deadbee", "deadbeef0", "deadbeeg", "rush_deadbeef"} {
		err := validateTokenID(id)
		if err == nil || err.Error() != "token ID must be exactly 8 lowercase hexadecimal characters" || strings.Contains(err.Error(), id) {
			t.Fatalf("invalid id %q error = %v", id, err)
		}
	}
}

func TestTokensInvocationIsCommandScoped(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"tokens", "list"}, true},
		{[]string{"run", "tokens"}, false},
		{[]string{"--name", "tokens", "run"}, false},
		{[]string{"--", "tokens"}, false},
	} {
		if got := isTokensInvocation(tc.args); got != tc.want {
			t.Errorf("isTokensInvocation(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestTokensCommandRegisteredOnRoot(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	command, _, err := rootCmd.Find([]string{"tokens"})
	if err != nil || command != tokensCmd {
		t.Fatalf("root lookup for tokens = %v, %v; want registered tokens command", command, err)
	}
}

func TestParseTokenExpiryRFC3339(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	now := mustParseTime(t, "2026-11-01T00:00:00Z")
	got, err := parseTokenExpiry("2026-12-01T12:30:00+02:00", now)
	want := mustParseTime(t, "2026-12-01T10:30:00Z")
	if err != nil || got == nil || !got.Equal(want) {
		t.Fatalf("RFC3339 expiry = %v, %v; want %v", got, err, want)
	}
}

func TestTokensCLIFullTokenRejectedWithoutLeak(t *testing.T) {
	base := t.TempDir()
	tokenTestEnv(t, base)
	code, out, errout := tokenTestCommandAt(t, base, "tokens", "create", "--name", "secret-test")
	if code != 0 {
		t.Fatalf("create exit=%d stderr=%q", code, errout)
	}
	secret := strings.TrimSpace(out)
	withoutPrefix := strings.TrimPrefix(secret, "rush_")
	_, secretPart, ok := strings.Cut(withoutPrefix, ".")
	if !ok || secretPart == "" {
		t.Fatalf("invalid token output %q", secret)
	}
	for _, verb := range []string{"show", "revoke"} {
		code, output, stderr, errorText := tokenTestCommandAtWithError(t, base, "tokens", verb, secret)
		if code != 2 {
			t.Fatalf("%s full-token exit=%d, want 2", verb, code)
		}
		for label, text := range map[string]string{"stdout": output, "stderr": stderr, "error": errorText} {
			if strings.Contains(text, secret) || strings.Contains(text, secretPart) {
				t.Fatalf("%s leaked token secret in %s: %q", verb, label, text)
			}
		}
	}
}

func TestTokensCLIExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"missing name", []string{"tokens", "create"}, 2},
		{"bad expiry", []string{"tokens", "create", "--name", "x", "--expires", "forever"}, 2},
		{"unknown scope", []string{"tokens", "create", "--name", "x", "--scope", "bogus"}, 2},
		{"auto without admin", []string{"tokens", "create", "--name", "x", "--permission", "auto"}, 2},
		{"invalid uppercase ID", []string{"tokens", "show", "DEADBEEF"}, 2},
		{"invalid long ID", []string{"tokens", "revoke", "deadbeef0"}, 2},
		{"unknown show", []string{"tokens", "show", "deadbeef"}, 4},
		{"unknown revoke", []string{"tokens", "revoke", "deadbeef"}, 4},
		{"unknown tokens flag", []string{"tokens", "list", "--not-a-flag"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := tokenTestCommand(t, tc.args...)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestTokensCLIDefaultScopeIsRun(t *testing.T) {
	base := t.TempDir()
	tokenTestEnv(t, base)
	code, _, stderr := tokenTestCommandAt(t, base, "tokens", "create", "--name", "x")
	if code != 0 {
		t.Fatalf("create exit=%d stderr=%q", code, stderr)
	}
	records, err := config.GlobalAPITokenStore().ListAPITokens()
	if err != nil || len(records) != 1 || len(records[0].Scopes) != 1 || records[0].Scopes[0] != "run" {
		t.Fatalf("stored scopes = %#v, err=%v; want [run]", records, err)
	}
}

func TestTokensCLIRepeatedScopeInvocationsAreIsolated(t *testing.T) {
	base := t.TempDir()
	tokenTestEnv(t, base)
	for _, tc := range []struct {
		name  string
		args  []string
		wants []string
	}{
		{"read", []string{"--scope", "read"}, []string{"read"}},
		{"config", []string{"--scope", "config"}, []string{"config"}},
		{"default", nil, []string{"run"}},
	} {
		args := append([]string{"tokens", "create", "--name", tc.name}, tc.args...)
		code, _, stderr := tokenTestCommandAt(t, base, args...)
		if code != 0 {
			t.Fatalf("create %s exit=%d stderr=%q", tc.name, code, stderr)
		}
		records, err := config.GlobalAPITokenStore().ListAPITokens()
		if err != nil {
			t.Fatalf("list tokens: %v", err)
		}
		var got []string
		for _, record := range records {
			if record.Name == tc.name {
				got = record.Scopes
				break
			}
		}
		if len(got) != len(tc.wants) {
			t.Fatalf("%s scopes=%v, want %v", tc.name, got, tc.wants)
		}
		for i := range got {
			if got[i] != tc.wants[i] {
				t.Fatalf("%s scopes=%v, want %v", tc.name, got, tc.wants)
			}
		}
	}
}

func TestTokensCLINonJSONStreams(t *testing.T) {
	base := t.TempDir()
	tokenTestEnv(t, base)
	code, out, stderr := tokenTestCommandAt(t, base, "tokens", "create", "--name", "plain")
	if code != 0 || stderr == "" || strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("create exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	plain := strings.TrimSuffix(out, "\n")
	if !regexp.MustCompile(`^rush_[0-9a-f]{8}\.[A-Za-z0-9_-]{43}$`).MatchString(plain) {
		t.Fatalf("plain output is not one parseable token: %q", out)
	}
	code, out, stderr = tokenTestCommandAt(t, base, "tokens", "list")
	if code != 0 || stderr != "" || !strings.Contains(out, "plain") {
		t.Fatalf("list exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
}

func TestTokensCLICreateJSONAndMetadataNeverExposeHash(t *testing.T) {
	base := t.TempDir()
	tokenTestEnv(t, base)
	code, out, errout := tokenTestCommandAt(t, base, "tokens", "create", "--name", "ci", "--json")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, errout)
	}
	var createdJSON map[string]any
	if err := json.Unmarshal([]byte(out), &createdJSON); err != nil {
		t.Fatalf("create stdout is not a JSON object: %v %q", err, out)
	}
	assertExactJSONKeys(t, createdJSON, []string{"token", "record"})
	var created struct {
		Token  string         `json:"token"`
		Record map[string]any `json:"record"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("create stdout is not JSON: %v %q", err, out)
	}
	assertExactJSONKeys(t, created.Record, []string{"id", "name", "scopes", "permission", "created"})
	if created.Token == "" || strings.Count(out, created.Token) != 1 || strings.Contains(errout, created.Token) {
		t.Fatalf("plaintext placement stdout=%q stderr=%q", out, errout)
	}
	records, err := config.GlobalAPITokenStore().ListAPITokens()
	if err != nil || len(records) != 1 {
		t.Fatalf("store records=%#v err=%v", records, err)
	}
	digest, secret := records[0].Hash, created.Token
	wantDigest := sha256.Sum256([]byte(secret))
	if digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("stored digest = %q, want SHA-256 of issued token", digest)
	}
	_, secretPart, ok := strings.Cut(strings.TrimPrefix(secret, "rush_"), ".")
	if !ok || secretPart == "" {
		t.Fatalf("invalid created token %q", secret)
	}
	for _, verb := range []struct {
		args []string
		keys []string
	}{
		{[]string{"tokens", "list", "--json"}, []string{"id", "name", "scopes", "permission", "created"}},
		{[]string{"tokens", "show", records[0].ID, "--json"}, []string{"id", "name", "scopes", "permission", "created"}},
	} {
		code, text, stderr := tokenTestCommandAt(t, base, verb.args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v exit=%d stderr=%q", verb.args, code, stderr)
		}
		checkObject := func(value map[string]any) {
			assertExactJSONKeys(t, value, verb.keys)
			for k := range value {
				if strings.Contains(strings.ToLower(k), "hash") || strings.Contains(strings.ToLower(k), "sha256") || strings.Contains(strings.ToLower(k), "secret") || strings.Contains(strings.ToLower(k), "digest") {
					t.Errorf("sensitive JSON key %q", k)
				}
			}
		}
		if verb.args[1] == "list" {
			var values []map[string]any
			if err := json.Unmarshal([]byte(text), &values); err != nil || len(values) == 0 {
				t.Fatalf("tokens list JSON must be a nonempty array: %q (%v)", text, err)
			}
			foundID := false
			for _, value := range values {
				checkObject(value)
				if value["id"] == records[0].ID {
					foundID = true
				}
			}
			if !foundID {
				t.Fatalf("tokens list JSON does not contain created id %q: %s", records[0].ID, text)
			}
		} else {
			var value map[string]any
			if err := json.Unmarshal([]byte(text), &value); err != nil || value == nil {
				t.Fatalf("tokens show JSON must be an object: %q (%v)", text, err)
			}
			checkObject(value)
		}
		for _, sensitive := range []string{digest, secret, secretPart} {
			if strings.Contains(text, sensitive) || strings.Contains(stderr, sensitive) {
				t.Fatalf("%v leaked sensitive value", verb.args)
			}
		}
	}
	emptyBase := t.TempDir()
	tokenTestEnv(t, emptyBase)
	code, empty, stderr := tokenTestCommandAt(t, emptyBase, "tokens", "list", "--json")
	if code != 0 || stderr != "" || strings.TrimSpace(empty) != "[]" {
		t.Fatalf("empty tokens list JSON = exit %d, stdout %q, stderr %q; want []", code, empty, stderr)
	}
}

func assertExactJSONKeys(t *testing.T, value map[string]any, expected []string) {
	t.Helper()
	if len(value) != len(expected) {
		t.Fatalf("JSON keys=%v want exactly %v", value, expected)
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			t.Fatalf("JSON key %q missing from %v", key, value)
		}
	}
}

func TestTokensCLIRevokeIsIdempotent(t *testing.T) {
	base := t.TempDir()
	tokenTestEnv(t, base)
	code, out, _ := tokenTestCommandAt(t, base, "tokens", "create", "--name", "ci")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	id, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(out), "rush_"), ".")
	if !ok {
		t.Fatalf("invalid token output %q", out)
	}
	for i := 0; i < 2; i++ {
		code, _, _ = tokenTestCommandAt(t, base, "tokens", "revoke", id)
		if code != 0 {
			t.Fatalf("revoke #%d exit %d", i+1, code)
		}
	}
}

func TestTokensCLISecretShapedValidationInputsDoNotLeak(t *testing.T) {
	base := t.TempDir()
	tokenTestEnv(t, base)
	secret := "rush_secret-shaped-value"
	for _, args := range [][]string{
		{"tokens", "create", "--name", "ok", "--scope", secret},
		{"tokens", "create", "--name", "ok", "--permission", secret},
		{"tokens", "create", "--name", "ok", "--expires", secret},
		{"tokens", "create", "--name", secret, "--scope", "unknown"},
	} {
		code, stdout, stderr, errorText := tokenTestCommandAtWithError(t, base, args...)
		if code != 2 {
			t.Fatalf("%v exit=%d want 2", args, code)
		}
		for label, text := range map[string]string{"stdout": stdout, "stderr": stderr, "error": errorText} {
			if strings.Contains(text, secret) {
				t.Fatalf("%v leaked in %s: %q", args, label, text)
			}
		}
	}
}

func TestTokensCLIDataDirAndProjectConfigAreIgnored(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(t *testing.T, base string) []string
	}{
		{"data-dir flag", func(t *testing.T, base string) []string {
			decoyDir := filepath.Join(base, "decoy-flag")
			seedTokenDecoy(t, decoyDir)
			return []string{"--data-dir", decoyDir, "tokens", "list"}
		}},
		{"project configs", func(t *testing.T, base string) []string {
			decoyDir := filepath.Join(base, "project-decoy")
			seedTokenDecoy(t, decoyDir)
			work := filepath.Join(base, "project")
			if err := os.MkdirAll(filepath.Join(work, ".rush"), 0o700); err != nil {
				t.Fatal(err)
			}
			contentsBytes, err := json.Marshal(struct {
				Options struct {
					DataDirectory string `json:"data_directory"`
				} `json:"options"`
			}{Options: struct {
				DataDirectory string `json:"data_directory"`
			}{DataDirectory: decoyDir}})
			if err != nil {
				t.Fatal(err)
			}
			contents := string(contentsBytes)
			for _, path := range []string{filepath.Join(work, "rush.json"), filepath.Join(work, ".rush", "rush.json")} {
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return []string{"tokens", "list"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			tokenTestEnv(t, base)
			args := tc.configure(t, base)
			var code int
			var stdout, stderr string
			if tc.name == "project configs" {
				projectCwd := filepath.Join(base, "project")
				code, stdout, stderr, _ = tokenTestCommandAtWithErrorInDir(t, projectCwd, args...)
			} else {
				code, stdout, stderr = tokenTestCommandAt(t, base, args...)
			}
			if code != 0 || stdout != "" || stderr != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			records, err := config.GlobalAPITokenStore().ListAPITokens()
			if err != nil || len(records) != 0 {
				t.Fatalf("global token store=%#v err=%v", records, err)
			}
			if _, ok, err := config.GlobalAPITokenStore().VerifyAPIToken(testTokenDecoy, time.Now()); err == nil || ok {
				t.Fatalf("decoy verified: ok=%v err=%v", ok, err)
			}
		})
	}
}

const testTokenDecoy = "rush_01020304.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func seedTokenDecoy(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(testTokenDecoy))
	doc := `{"version":1,"tokens":[{"id":"01020304","name":"decoy","scopes":["read"],"permission":"ask","created":"2030-01-01T00:00:00Z","sha256":"` + hex.EncodeToString(digest[:]) + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "api-tokens.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTokensCLIWrappedNotFoundUsesFour(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	err := tokenStoreError(fmt.Errorf("wrapped: %w", config.ErrAPITokenNotFound))
	code, ok := tokensExitCode([]string{"tokens", "show", "deadbeef"}, err)
	if !ok || code != 4 {
		t.Fatalf("wrapped not-found exit = %d, %v; want 4, true", code, ok)
	}
}

func TestTokensCLIStoreIOErrorIsOne(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	base := t.TempDir()
	tokenTestEnv(t, base)
	path := config.GlobalAPITokenStore().Path
	code, _, stderr := tokenTestCommandAt(t, base, "tokens", "list")
	if code != 0 {
		t.Fatalf("tokens list before fault exit=%d stderr=%q; want 0", code, stderr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || path != config.GlobalAPITokenStore().Path {
		t.Fatalf("fault path %q does not match store path %q or is not a directory: info=%v err=%v", path, config.GlobalAPITokenStore().Path, info, err)
	}
	for _, args := range [][]string{{"tokens", "list"}, {"tokens", "show", "deadbeef"}, {"tokens", "create", "--name", "x"}, {"tokens", "revoke", "deadbeef"}} {
		code, _, _, errorText := tokenTestCommandAtWithError(t, base, args...)
		if code != 1 || errorText != "invalid API token store file" {
			t.Errorf("%v exit = %d error=%q, want exit 1 and %q", args, code, errorText, "invalid API token store file")
		}
	}
}
