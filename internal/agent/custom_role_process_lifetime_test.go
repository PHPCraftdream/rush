package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const (
	roleProcessHelperTest = "TestCustomRoleProcessLifetimeHelper"
	roleProcessName       = "analyst"
	roleProcessLogicalID  = "role-process-logical-call"
	roleProcessQueueID    = "role-process-durable-row"
)

type roleProcessArtifact struct {
	PID      int    `json:"pid"`
	RootID   string `json:"root_id"`
	ChildID  string `json:"child_id"`
	Logical  string `json:"logical_call_id"`
	QueueID  string `json:"queue_id"`
	Model    string `json:"resolved_model,omitempty"`
	Dispatch string `json:"dispatch,omitempty"`
}

// TestCustomRoleProcessLifetimeDurableDispatch starts independent writer and
// reader test-binary lifetimes against one SQLite database. The reader obtains
// both role bindings from SQLite, decodes the v3 durable call-options row, then
// rebuilds and exercises the coordinator's native tool policy. Helpers are
// fake test-binary subprocesses; the parent does not run an application
// command, provider request, or model call. The writer helper migrates the
// workspace config, and later helpers reload that active disk generation.
func TestCustomRoleProcessLifetimeDurableDispatch(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	dbDir := filepath.Join(root, "durable-db")
	globalConfig := filepath.Join(root, "global-config")
	globalData := filepath.Join(root, "global-data")
	for _, dir := range []string{workDir, dbDir, globalConfig, globalData} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	proofPath := filepath.Join(workDir, "process-role-proof.txt")
	require.NoError(t, os.WriteFile(proofPath, []byte("durable role process dispatch\n"), 0o600))
	writeRoleProcessConfig(t, workDir, "old-model", true)

	writerPath := filepath.Join(root, "writer.json")
	writerOutput := runRoleProcessHelper(t, "writer", writerPath, "", workDir, dbDir, globalConfig, globalData, proofPath)
	writerData, err := os.ReadFile(writerPath)
	require.NoError(t, err, "writer helper result: %s", writerOutput)
	var writer roleProcessArtifact
	require.NoError(t, json.Unmarshal(writerData, &writer), "writer helper result: %s", writerOutput)
	require.Positive(t, writer.PID)
	require.NotEmpty(t, writer.RootID)
	require.NotEmpty(t, writer.ChildID)
	require.FileExists(t, filepath.Join(workDir, "rush.ktav"), "writer process should migrate the workspace config")

	// The later process must load the edited, valid disk generation. Removing
	// the live tools field and changing the model cannot mutate the DB snapshot.
	writeRoleProcessConfig(t, workDir, "refreshed-model", false)
	assertRoleProcessConfig(t, workDir, "refreshed-model", false)
	readerPath := filepath.Join(root, "reader.json")
	readerOutput := runRoleProcessHelper(t, "reader", readerPath, writerPath, workDir, dbDir, globalConfig, globalData, proofPath)
	readerData, err := os.ReadFile(readerPath)
	require.NoError(t, err, "reader helper result: %s", readerOutput)
	var reader roleProcessArtifact
	require.NoError(t, json.Unmarshal(readerData, &reader), "reader helper result: %s", readerOutput)
	require.Positive(t, reader.PID)
	require.NotEqual(t, writer.PID, reader.PID, "writer and reader must be separate process lifetimes")
	require.Equal(t, writer.RootID, reader.RootID)
	require.Equal(t, writer.ChildID, reader.ChildID)
	require.Equal(t, "refreshed-model", reader.Model)
	require.Equal(t, "view-allowed;write-denied;grep-denied", reader.Dispatch)

	// A third fresh coordinator verifies removal fails during owning role
	// restoration, before model construction or any child-agent execution.
	writeRoleProcessConfig(t, workDir, "refreshed-model", false)
	removeRoleProcessRole(t, workDir)
	assertRoleProcessRoleRemoved(t, workDir)
	removedPath := filepath.Join(root, "removed.json")
	removedOutput := runRoleProcessHelper(t, "removed", removedPath, writerPath, workDir, dbDir, globalConfig, globalData, proofPath)
	removedData, err := os.ReadFile(removedPath)
	require.NoError(t, err, "removed-role helper result: %s", removedOutput)
	var removed roleProcessArtifact
	require.NoError(t, json.Unmarshal(removedData, &removed), "removed-role helper result: %s", removedOutput)
	require.NotEqual(t, reader.PID, removed.PID)
	require.Equal(t, writer.RootID, removed.RootID)
	require.Equal(t, "removed-before-model-build", removed.Dispatch)
}

// TestCustomRoleProcessLifetimeHelper is invoked only by the parent test in a
// bounded helper process. It deliberately accepts no role-contract JSON or
// policy arguments: the reader can only recover those from the shared DB.
func TestCustomRoleProcessLifetimeHelper(t *testing.T) {
	mode := os.Getenv("RUSH_ROLE_PROCESS_HELPER")
	if mode == "" {
		return
	}
	workDir := os.Getenv("RUSH_ROLE_PROCESS_WORKDIR")
	dbDir := os.Getenv("RUSH_ROLE_PROCESS_DBDIR")
	globalConfig := os.Getenv("RUSH_ROLE_PROCESS_GLOBAL_CONFIG")
	globalData := os.Getenv("RUSH_ROLE_PROCESS_GLOBAL_DATA")
	proofPath := os.Getenv("RUSH_ROLE_PROCESS_PROOF")
	resultPath := os.Getenv("RUSH_ROLE_PROCESS_RESULT")
	for key, value := range map[string]string{
		"workdir": workDir, "database directory": dbDir,
		"global config": globalConfig, "global data": globalData,
		"proof file": proofPath, "result file": resultPath,
	} {
		require.NotEmpty(t, value, "missing helper %s", key)
	}
	require.NotEqual(t, globalConfig, globalData, "global config and data paths must be distinct")
	require.NoError(t, os.MkdirAll(globalConfig, 0o755))
	require.NoError(t, os.MkdirAll(globalData, 0o755))
	t.Setenv("RUSH_GLOBAL_CONFIG", globalConfig)
	t.Setenv("RUSH_GLOBAL_DATA", globalData)
	t.Setenv("XDG_CONFIG_HOME", globalConfig)
	t.Setenv("XDG_DATA_HOME", globalData)
	t.Setenv("HOME", filepath.Dir(globalConfig))
	t.Setenv("USERPROFILE", filepath.Dir(globalConfig))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	conn, queries, sessions := openRoleProcessDatabase(t, ctx, dbDir)
	cfg, err := config.Init(workDir, filepath.Join(workDir, "config-data"), false)
	require.NoError(t, err)
	activeConfig := filepath.Join(workDir, "rush.ktav")
	activeInfo, err := os.Stat(activeConfig)
	require.NoError(t, err, "active workspace config must remain present")
	loadedActiveConfig := false
	for _, loadedPath := range cfg.LoadedPaths() {
		loadedInfo, statErr := os.Stat(loadedPath)
		if statErr == nil && os.SameFile(loadedInfo, activeInfo) {
			loadedActiveConfig = true
			break
		}
	}
	require.True(t, loadedActiveConfig, "new process must have reloaded the active workspace rush.ktav config")
	q := queries
	messages := message.NewService(q)
	permissions := permission.NewPermissionService(ctx, workDir, true, nil, q)
	hist := history.NewService(q, conn)
	tracker := filetracker.NewService(q)
	t.Cleanup(func() {
		messages.Shutdown()
		hist.(interface{ Shutdown() }).Shutdown()
		permissions.(interface{ Shutdown() }).Shutdown()
	})

	result := roleProcessArtifact{PID: os.Getpid(), QueueID: roleProcessQueueID}
	switch mode {
	case "writer":
		root, createErr := sessions.Create(ctx, "process-lifetime custom-role root")
		require.NoError(t, createErr)
		result.RootID = root.ID
		ancestorTools := []string{"view", "write"}
		ancestor := &session.RoleContract{
			Version: session.RoleContractVersion,
			Tools:   &ancestorTools,
			Inherited: [][]string{
				{"view", "write"},
				{"view", "grep"},
			},
		}
		rootContract, resolveErr := ResolveRoleContract(cfg.Config(), roleProcessName, ancestor)
		require.NoError(t, resolveErr)
		result.Logical = roleProcessLogicalID
		rootCall := SessionAgentCall{
			SessionID: root.ID, LogicalCallID: roleProcessLogicalID,
			RequestedModelRole: RequestedModelRole(roleProcessName),
			RoleContract:       rootContract.Clone(),
			CallOptions:        &CallOptions{RoleContract: rootContract.Clone()},
		}
		require.NoError(t, bindRootRoleContract(ctx, sessions, rootCall))
		childID := sessions.CreateAgentToolSessionID("process-role-message", "process-role-tool-call")
		child, createErr := sessions.CreateTaskSession(ctx, childID, root.ID, "process-lifetime custom-role child")
		require.NoError(t, createErr)
		result.ChildID = child.ID
		childContract, resolveErr := ResolveRoleContract(cfg.Config(), roleProcessName, rootContract)
		require.NoError(t, resolveErr)
		require.Len(t, rootContract.Inherited, 3)
		require.Len(t, childContract.Inherited, 4, "each inherited ceiling remains an independent intersection")
		require.NoError(t, sessions.(session.SessionRoleContractStore).BindSessionRoleContract(ctx, child.ID, childContract))
		callData := ToSessionAgentCallData(rootCall)
		require.NoError(t, sessions.EnqueueRunQueueEntry(ctx, roleProcessQueueID, root.ID, roleProcessJSON(t, callData)))
	case "reader", "removed":
		result = readRoleProcessMetadata(t, os.Getenv("RUSH_ROLE_PROCESS_BOOTSTRAP"))
		result.PID = os.Getpid()
		entry, readErr := sessions.GetRunQueueEntry(ctx, result.QueueID)
		require.NoError(t, readErr)
		require.NotNil(t, entry)
		var durable session.SessionAgentCallData
		require.NoError(t, json.Unmarshal([]byte(entry.CallData), &durable))
		require.NotNil(t, durable.CallOptionsSpec)
		require.Equal(t, session.CallOptionsSpecVersion, durable.CallOptionsSpec.Version, "role call-options must remain v3 on disk")
		rebuilt, decodeErr := FromSessionAgentCallData(durable)
		require.NoError(t, decodeErr)
		require.Equal(t, result.RootID, rebuilt.SessionID)
		require.Equal(t, result.Logical, rebuilt.LogicalCallID)

		queuedTools := []string{"view", "write"}
		queuedExpected := &session.RoleContract{
			Version: session.RoleContractVersion,
			Name:    roleProcessName,
			Model:   session.ModelCfg{Provider: "role-fixture", Model: "old-model"},
			Tools:   &queuedTools,
			Inherited: [][]string{
				{"view", "write"},
				{"view", "grep"},
				{"view", "write"},
			},
		}
		require.NotNil(t, durable.RoleContract, "v3 queued call must carry its standalone role contract")
		require.NotNil(t, durable.CallOptionsSpec.RoleContract, "v3 queued options must carry their role contract")
		require.Equal(t, queuedExpected, durable.RoleContract, "standalone queued contract must preserve expected identity, model, and caps")
		require.Equal(t, queuedExpected, durable.CallOptionsSpec.RoleContract, "queued options contract must preserve expected identity, model, and caps")
		require.NotNil(t, rebuilt.RoleContract)
		require.NotNil(t, rebuilt.CallOptions)
		require.NotNil(t, rebuilt.CallOptions.RoleContract)
		require.Equal(t, queuedExpected, rebuilt.RoleContract)
		require.Equal(t, queuedExpected, rebuilt.CallOptions.RoleContract)

		rootStore, ok := sessions.(session.SessionRootRoleContractStore)
		require.True(t, ok, "session service must expose durable root role contracts")
		storedLogicalID, storedRootContract, found, contractErr := rootStore.GetRootRoleContract(ctx, result.RootID)
		require.NoError(t, contractErr)
		require.True(t, found, "root binding must exist in the durable root-contract store")
		require.Equal(t, result.Logical, storedLogicalID, "root binding must belong to the queued logical call")
		require.NotNil(t, storedRootContract)
		require.Equal(t, queuedExpected, storedRootContract, "durable root binding must preserve expected identity, model, and caps")
		require.Equal(t, storedRootContract, durable.RoleContract, "standalone queued contract must match the independently read root binding")
		require.Equal(t, storedRootContract, durable.CallOptionsSpec.RoleContract, "queued options contract must match the independently read root binding")

		identityCall := SessionAgentCall{
			SessionID: rebuilt.SessionID, LogicalCallID: rebuilt.LogicalCallID,
			CallOptions: &CallOptions{},
		}
		require.Nil(t, identityCall.RoleContract, "restoration input must not supply the queued contract")
		require.Nil(t, identityCall.CallOptions.RoleContract, "restoration input must not supply queued role options")
		rootCall, restoreErr := restoreRootRoleContract(ctx, sessions, identityCall)
		require.NoError(t, restoreErr)
		require.NotNil(t, rootCall.RoleContract, "owning restore must load the durable root binding")
		require.NotNil(t, rootCall.CallOptions)
		require.NotNil(t, rootCall.CallOptions.RoleContract)
		require.Equal(t, storedRootContract, rootCall.RoleContract, "restored root contract must come from the durable binding")
		require.Equal(t, storedRootContract, rootCall.CallOptions.RoleContract, "restored options contract must come from the durable binding")
		childContract, found, contractErr := sessions.(session.SessionRoleContractStore).GetSessionRoleContract(ctx, result.ChildID)
		require.NoError(t, contractErr)
		require.True(t, found, "child binding must be read from the durable child-contract store")
		require.Equal(t, roleProcessName, childContract.Name)
		require.Equal(t, []string{"view", "write"}, *childContract.Tools)
		require.Len(t, childContract.Inherited, 4, "the child must retain each inherited intersection")
		require.Equal(t, [][]string{{"view", "write"}, {"view", "grep"}, {"view", "write"}, {"view", "write"}}, childContract.Inherited)

		coord := &coordinator{
			cfg: cfg, sessions: sessions, messages: messages, permissions: permissions,
			history: hist, filetracker: tracker,
		}
		roleCtx := WithSelectedRoleContract(WithCallOptions(ctx, &CallOptions{RoleContract: rootCall.RoleContract.Clone()}), rootCall.RoleContract)
		roleOptions := callOptionsFrom(roleCtx)
		t.Cleanup(roleOptions.buildScope.close)
		if mode == "removed" {
			_, configured := cfg.Config().Roles[roleProcessName]
			require.False(t, configured)
			_, resolveErr := coord.resolveRequestedDelegationModel(roleCtx, cfg.Config(), result.RootID, RequestedModelRole(roleProcessName))
			require.ErrorIs(t, resolveErr, ErrAgentRoleUnconfigured)
			result.Dispatch = "removed-before-model-build"
			require.NoError(t, writeRoleProcessArtifact(resultPath, result))
			return
		}

		selected, resolveErr := coord.resolveRequestedDelegationModel(roleCtx, cfg.Config(), result.RootID, RequestedModelRole(roleProcessName))
		require.NoError(t, resolveErr)
		require.Equal(t, "refreshed-model", selected.Model.ModelCfg.Model, "owning role resolution must use the valid refreshed disk profile")
		result.Model = selected.Model.ModelCfg.Model
		base := config.Agent{AllowedTools: []string{"view", "write", "grep"}, AllowedMCP: map[string][]string{}}
		options := &CallOptions{RoleContract: childContract.Clone()}
		policyCtx := WithCallOptions(ctx, options)
		t.Cleanup(options.buildScope.close)
		built, buildErr := coord.buildPolicyTools(policyCtx, cfg.Config(), base, false)
		require.NoError(t, buildErr)
		require.Equal(t, []string{"view"}, pinnedToolNames(built.tools), "live removal of the role tools field cannot widen its durable ceiling")
		view, resolveErr := built.leaves.resolve("view")
		require.NoError(t, resolveErr)
		viewInput, marshalErr := json.Marshal(map[string]string{"file_path": proofPath})
		require.NoError(t, marshalErr)
		dispatchCtx := context.WithValue(policyCtx, tools.SessionIDContextKey, result.RootID)
		viewResponse, runErr := view.Run(dispatchCtx, fantasy.ToolCall{ID: "durable-view", Name: "view", Input: string(viewInput)})
		require.NoError(t, runErr)
		require.False(t, viewResponse.IsError, "%s", viewResponse.Content)
		require.Contains(t, viewResponse.Content, "durable role process dispatch")

		_, resolveErr = built.leaves.resolve("write")
		require.Error(t, resolveErr, "inherited role intersections must deny the native write leaf")
		_, resolveErr = built.leaves.resolve("grep")
		require.Error(t, resolveErr, "the original role tool list must continue to deny grep")
		unrestricted, buildErr := coord.buildPolicyTools(context.Background(), cfg.Config(), base, false)
		require.NoError(t, buildErr)
		writeLeaf, resolveErr := unrestricted.leaves.resolve("write")
		require.NoError(t, resolveErr)
		guardedWrite := roleGuardTool{AgentTool: writeLeaf, contracts: []*session.RoleContract{childContract}}
		deniedInput, marshalErr := json.Marshal(map[string]string{
			"file_path": filepath.Join(workDir, "forbidden-role-write.txt"),
			"content":   "this must not be written",
		})
		require.NoError(t, marshalErr)
		denied, runErr := guardedWrite.Run(dispatchCtx, fantasy.ToolCall{ID: "durable-denied-write", Name: "write", Input: string(deniedInput)})
		require.NoError(t, runErr)
		require.True(t, denied.IsError)
		require.Contains(t, denied.Content, ErrRoleToolDenied.Error())
		_, statErr := os.Stat(filepath.Join(workDir, "forbidden-role-write.txt"))
		require.True(t, errors.Is(statErr, os.ErrNotExist), "denied native dispatch must not create files")
		result.Dispatch = "view-allowed;write-denied;grep-denied"
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	require.NoError(t, writeRoleProcessArtifact(resultPath, result))
}

func writeRoleProcessConfig(t *testing.T, workDir, model string, includeTools bool) {
	t.Helper()
	noDiscovery := false
	role := config.CustomRole{SelectedModel: config.SelectedModel{Provider: "role-fixture", Model: model}}
	if includeTools {
		allowed := []string{"view", "write"}
		role.Tools = &allowed
	}
	doc := map[string]any{
		"options": map[string]any{"disable_default_providers": true},
		"providers": map[string]config.ProviderConfig{
			"role-fixture": {
				ID: "role-fixture", Type: "openai-compat", APIKey: "fixture-only",
				BaseURL:            "https://example.invalid/v1",
				AutoDiscoverModels: &noDiscovery,
				Models: []catwalk.Model{
					{ID: "old-model", ContextWindow: 8192, DefaultMaxTokens: 1024},
					{ID: "refreshed-model", ContextWindow: 8192, DefaultMaxTokens: 1024},
				},
			},
		},
		"models": map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeSmart: {Provider: "role-fixture", Model: model},
			config.SelectedModelTypeFast:  {Provider: "role-fixture", Model: model},
		},
		"roles": map[string]config.CustomRole{roleProcessName: role},
	}
	writeRoleProcessConfigDocument(t, workDir, doc)
}

func assertRoleProcessConfig(t *testing.T, workDir, model string, expectTools bool) {
	t.Helper()
	_, doc := readRoleProcessConfigDocument(t, workDir)
	var roles map[string]config.CustomRole
	var models map[config.SelectedModelType]config.SelectedModel
	require.NoError(t, json.Unmarshal(doc["roles"], &roles))
	require.NoError(t, json.Unmarshal(doc["models"], &models))
	require.Equal(t, model, roles[roleProcessName].Model)
	require.Equal(t, model, models[config.SelectedModelTypeSmart].Model)
	if expectTools {
		require.NotNil(t, roles[roleProcessName].Tools)
		require.Equal(t, []string{"view", "write"}, *roles[roleProcessName].Tools)
	} else {
		require.Nil(t, roles[roleProcessName].Tools)
	}
}

func assertRoleProcessRoleRemoved(t *testing.T, workDir string) {
	t.Helper()
	_, doc := readRoleProcessConfigDocument(t, workDir)
	var roles map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(doc["roles"], &roles))
	require.NotContains(t, roles, roleProcessName, "the active Ktav document must contain the role removal")
}

func removeRoleProcessRole(t *testing.T, workDir string) {
	t.Helper()
	path, doc := readRoleProcessConfigDocument(t, workDir)
	doc["roles"] = json.RawMessage(`{}`)
	writeRoleProcessConfigDocumentAt(t, path, doc)
}

func writeRoleProcessConfigDocument(t *testing.T, workDir string, doc map[string]any) {
	t.Helper()
	path, current := readRoleProcessConfigDocument(t, workDir)
	if current == nil {
		current = make(map[string]json.RawMessage)
	}
	for key, value := range doc {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		current[key] = data
	}
	writeRoleProcessConfigDocumentAt(t, path, current)
}

func readRoleProcessConfigDocument(t *testing.T, workDir string) (string, map[string]json.RawMessage) {
	t.Helper()
	jsonPath := filepath.Join(workDir, "rush.json")
	ktavPath := filepath.Join(workDir, "rush.ktav")
	path := jsonPath
	if _, err := os.Stat(ktavPath); err == nil {
		path = ktavPath
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	require.NoError(t, err)
	if filepath.Ext(path) == ".ktav" {
		tree, decodeErr := ktavcodec.DecodeKtav(data)
		require.NoError(t, decodeErr)
		data, err = ktavcodec.ToJSON(tree)
		require.NoError(t, err)
	}
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &doc))
	return path, doc
}

func writeRoleProcessConfigDocumentAt(t *testing.T, path string, doc map[string]json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	if filepath.Ext(path) == ".ktav" {
		data, err = ktavcodec.ConvertJSONToKtav(data)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func openRoleProcessDatabase(t *testing.T, ctx context.Context, dbDir string) (*sql.DB, *db.Queries, session.Service) {
	t.Helper()
	conn, err := db.Connect(ctx, dbDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		if releaseErr := db.Release(dbDir); releaseErr != nil {
			t.Errorf("release helper database: %v", releaseErr)
		}
	})
	q := db.New(conn)
	return conn, q, session.NewService(q, conn)
}

func runRoleProcessHelper(t *testing.T, mode, result, bootstrap, workDir, dbDir, globalConfig, globalData, proofPath string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := platform.Command(ctx, executable, "-test.run=^"+roleProcessHelperTest+"$")
	cmd.Dir = workDir
	cmd.Env = roleProcessHelperEnvironment(map[string]string{
		"RUSH_ROLE_PROCESS_HELPER":        mode,
		"RUSH_ROLE_PROCESS_RESULT":        result,
		"RUSH_ROLE_PROCESS_BOOTSTRAP":     bootstrap,
		"RUSH_ROLE_PROCESS_WORKDIR":       workDir,
		"RUSH_ROLE_PROCESS_DBDIR":         dbDir,
		"RUSH_ROLE_PROCESS_GLOBAL_CONFIG": globalConfig,
		"RUSH_ROLE_PROCESS_GLOBAL_DATA":   globalData,
		"RUSH_ROLE_PROCESS_PROOF":         proofPath,
	})
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s helper failed (bounded stdout/stderr): %s", mode, output)
	data, err := os.ReadFile(result)
	require.NoError(t, err, "%s helper did not write its result", mode)
	return data
}

func roleProcessHelperEnvironment(extra map[string]string) []string {
	allowed := map[string]bool{
		"PATH": true, "SYSTEMROOT": true, "TEMP": true, "TMP": true,
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && allowed[strings.ToUpper(key)] {
			env = append(env, entry)
		}
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return append(env,
		"RUSH_GLOBAL_CONFIG="+extra["RUSH_ROLE_PROCESS_GLOBAL_CONFIG"],
		"RUSH_GLOBAL_DATA="+extra["RUSH_ROLE_PROCESS_GLOBAL_DATA"],
		"XDG_CONFIG_HOME="+extra["RUSH_ROLE_PROCESS_GLOBAL_CONFIG"],
		"XDG_DATA_HOME="+extra["RUSH_ROLE_PROCESS_GLOBAL_DATA"],
		"HOME="+filepath.Dir(extra["RUSH_ROLE_PROCESS_GLOBAL_CONFIG"]),
		"USERPROFILE="+filepath.Dir(extra["RUSH_ROLE_PROCESS_GLOBAL_CONFIG"]),
	)
}

func readRoleProcessMetadata(t *testing.T, path string) roleProcessArtifact {
	t.Helper()
	// The helper's bootstrap metadata contains only row identities, never a
	// contract or tool policy. Contract authority always comes from SQLite.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var result roleProcessArtifact
	require.NoError(t, json.Unmarshal(data, &result))
	require.NotEmpty(t, result.RootID)
	require.NotEmpty(t, result.ChildID)
	return result
}

func writeRoleProcessArtifact(path string, value roleProcessArtifact) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func roleProcessJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
