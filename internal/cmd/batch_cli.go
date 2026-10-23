package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var (
	batchCmd     = &cobra.Command{Use: "batch", Short: "Run and inspect durable job batches"}
	batchRunCmd  = &cobra.Command{Use: "run", Args: cobra.NoArgs, RunE: batchRun}
	batchShowCmd = &cobra.Command{Use: "show <root-id|prefix>", Args: cobra.ExactArgs(1), RunE: batchShow}
	batchAddCmd  = &cobra.Command{Use: "add <root-or-parent>", Args: cobra.ExactArgs(1), RunE: batchAdd}
	batchStopCmd = &cobra.Command{Use: "stop <root-id|node-id>", Args: cobra.ExactArgs(1), RunE: batchStop}
)

func init() {
	batchRunCmd.Flags().StringP("spec", "f", "-", "BatchSpec JSON file (default stdin)")
	batchRunCmd.Flags().String("session", "", "Owner session ID (created when omitted)")
	batchRunCmd.Flags().Bool("json", false, "Emit structured JSON")
	batchRunCmd.Flags().Bool("detach", false, "Request detached execution (unsupported)")
	batchAddCmd.Flags().StringP("spec", "f", "-", "Items JSON file (default stdin)")
	batchStopCmd.Flags().Duration("wait", 0, "Wait duration for owner acknowledgement; zero reports queued")
	batchShowCmd.Flags().Bool("json", false, "Emit structured JSON")
	batchStopCmd.Flags().Bool("json", false, "Emit structured JSON")
	batchCmd.AddCommand(batchRunCmd, batchShowCmd, batchAddCmd, batchStopCmd)
	rootCmd.AddCommand(batchCmd)
}

func batchApp(cmd *cobra.Command, owner bool) (*app.App, error) {
	a, err := setupApp(cmd)
	if err != nil {
		return nil, err
	}
	if owner && !a.BatchOwnerRuntime() {
		a.Shutdown()
		return nil, errors.New("batch add is owner-process-only; use 'rush batch stop' from another process")
	}
	return a, nil
}

func batchRun(cmd *cobra.Command, _ []string) error {
	detach, _ := cmd.Flags().GetBool("detach")
	if detach {
		return errors.New("--detach is unsupported: no durable external host is available")
	}
	spec, err := parseBatchSpec(cmd)
	if err != nil {
		return err
	}
	a, err := batchApp(cmd, false)
	if err != nil {
		return err
	}
	defer a.Shutdown()
	if !a.BatchOwnerRuntime() {
		return errors.New("batch run requires an owning runtime; detached execution is unavailable")
	}
	id, _ := cmd.Flags().GetString("session")
	sess, err := a.BatchCLISession(cmd.Context(), id)
	if err != nil {
		return err
	}
	a.Permissions.AutoApproveSession(sess.ID)
	root, err := a.BatchSubmit(cmd.Context(), sess.ID, spec)
	if err != nil {
		return err
	}
	if j, _ := cmd.Flags().GetBool("json"); j {
		if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "batch root: %s\n", root); err != nil {
			return err
		}
		stored, waitErr := a.BatchWait(cmd.Context(), sess.ID, root)
		if waitErr != nil {
			return waitErr
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"root_id": root, "owner_session_id": sess.ID, "state": stored.State})
	}
	if _, err = fmt.Fprintln(cmd.OutOrStdout(), root); err != nil {
		return err
	}
	_, err = a.BatchWait(cmd.Context(), sess.ID, root)
	return err
}

func batchReadStore(cmd *cobra.Command) (*session.BatchStore, *session.BatchLiveReader, func(), error) {
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	dataDir, err := config.ResolveDataDirectory(cwd, dataDirFlag)
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := db.Connect(cmd.Context(), dataDir)
	if err != nil {
		return nil, nil, nil, err
	}
	read, err := db.ConnectRead(cmd.Context(), dataDir)
	if err != nil {
		_ = db.ReleaseConn(conn)
		return nil, nil, nil, err
	}
	closeStore := func() { _ = db.ReleaseConn(read); _ = db.ReleaseConn(conn) }
	return session.NewBatchStore(conn), session.NewBatchLiveReader(db.New(read), dataDir), closeStore, nil
}

type (
	batchShowNode struct {
		NodeID   string `json:"node_id"`
		ParentID string `json:"parent_id"`
		Depth    int    `json:"depth"`
		Kind     string `json:"kind"`
		State    string `json:"state"`
	}
	batchShowTree struct {
		RootID         string          `json:"root_id"`
		OwnerSessionID string          `json:"owner_session_id"`
		HostID         string          `json:"host_id"`
		Incarnation    string          `json:"incarnation"`
		Complete       bool            `json:"complete"`
		Nodes          []batchShowNode `json:"nodes"`
	}
)

func findBatchCLIIdentity(ctx context.Context, store *session.BatchStore, target string) (session.BatchCLIIdentity, string, error) {
	identity, nodeID, err := store.BatchCLIFind(ctx, target)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return session.BatchCLIIdentity{}, "", fmt.Errorf("batch tables are unavailable in the configured database: %w", err)
	}
	return identity, nodeID, err
}

func renderBatchShow(identity session.BatchCLIIdentity, level session.BatchLevelResult) (batchShowTree, error) {
	if !level.Complete {
		return batchShowTree{}, errors.New("batch tree is incomplete or host status unknown")
	}
	tree := batchShowTree{RootID: identity.Root, OwnerSessionID: identity.Owner, HostID: identity.Host, Incarnation: identity.Runner, Complete: level.Complete}
	for _, node := range level.Subtree {
		tree.Nodes = append(tree.Nodes, batchShowNode{NodeID: node.NodeID, ParentID: node.ParentID, Depth: node.Depth, Kind: node.Kind, State: node.State})
	}
	return tree, nil
}

func batchShow(cmd *cobra.Command, args []string) error {
	store, reader, closeStore, err := batchReadStore(cmd)
	if err != nil {
		return err
	}
	defer closeStore()
	identity, nodeID, err := findBatchCLIIdentity(cmd.Context(), store, args[0])
	if err != nil {
		return err
	}
	if nodeID != identity.Root {
		return errors.New("batch show requires a root id or root prefix")
	}
	level := reader.BatchLevel(cmd.Context(), session.BatchLiveRef{OwnerSessionID: identity.Owner, RootID: identity.Root}, identity.Root)
	tree, err := renderBatchShow(identity, level)
	if err != nil {
		return err
	}
	if j, _ := cmd.Flags().GetBool("json"); j {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(tree)
	}
	if _, err = fmt.Fprintf(cmd.OutOrStdout(), "owner_session_id=%s host_id=%s incarnation=%s\n", identity.Owner, identity.Host, identity.Runner); err != nil {
		return err
	}
	for _, node := range tree.Nodes {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s%s %s %s\n", strings.Repeat("  ", node.Depth), node.NodeID, node.Kind, node.State); err != nil {
			return err
		}
	}
	return nil
}

type batchCLIAppContextKey struct{}

func batchAdd(cmd *cobra.Command, args []string) error {
	if owner, ok := cmd.Context().Value(batchCLIAppContextKey{}).(*app.App); ok && owner != nil {
		if !owner.BatchOwnerRuntime() {
			return errors.New("batch add is owner-process-only; use 'rush batch stop' from another process")
		}
		spec, err := parseBatchSpec(cmd)
		if err != nil {
			return err
		}
		if spec.Batch == nil {
			return errors.New("batch add spec must contain batch items")
		}
		return owner.BatchAdd(cmd.Context(), args[0], args[0], spec.Batch.Items)
	}
	// A CLI invocation is a separate process and cannot possess the owner's runtime.
	store, _, closeStore, err := batchReadStore(cmd)
	if err == nil {
		defer closeStore()
		_, _, err = findBatchCLIIdentity(cmd.Context(), store, args[0])
		if err != nil {
			return err
		}
	}
	return errors.New("batch add is owner-process-only; use 'rush batch stop' for cross-process control")
}

func batchStop(cmd *cobra.Command, args []string) error {
	store, reader, closeStore, err := batchReadStore(cmd)
	if err != nil {
		return err
	}
	defer closeStore()
	wait, _ := cmd.Flags().GetDuration("wait")
	if wait < 0 {
		return errors.New("--wait must be nonnegative")
	}
	if wait > 30*time.Second {
		return errors.New("--wait exceeds 30s limit")
	}
	identity, nodeID, err := findBatchCLIIdentity(cmd.Context(), store, args[0])
	if err != nil {
		return err
	}
	target := session.BatchControlTarget{Owner: identity.Owner, Host: identity.Host, Root: identity.Root, Node: nodeID}
	req, err := store.BatchRequestStop(cmd.Context(), target, session.BatchSharedHostProbe(reader.DataDir))
	if err != nil {
		return err
	}
	state, answerCode := "queued", ""
	if wait > 0 {
		ctx, cancel := context.WithTimeout(cmd.Context(), wait)
		defer cancel()
		ack, pollErr := store.BatchCLIPollControl(ctx, target, req.RequestID, wait)
		if pollErr == nil && ack.AnswerCode != "" {
			if ack.AnswerCode != "accepted" {
				return fmt.Errorf("stop request was not accepted: %s", ack.AnswerCode)
			}
			state, answerCode = "applied", ack.AnswerCode
		} else if pollErr != nil && !errors.Is(pollErr, context.DeadlineExceeded) {
			return pollErr
		}
	}
	result := map[string]any{"request_id": req.RequestID, "state": state, "root_id": identity.Root, "node_id": nodeID}
	if answerCode != "" {
		result["answer_code"] = answerCode
	}
	if j, _ := cmd.Flags().GetBool("json"); j {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "stop %s: request %s\n", state, req.RequestID)
	return err
}
