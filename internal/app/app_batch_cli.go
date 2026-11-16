package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/google/uuid"
)

type BatchCLIStore struct {
	Store  *session.BatchStore
	Reader *session.BatchLiveReader
}

func batchCLIStore(p batchCLIParams) (*BatchCLIStore, error) {
	if p.env.DB == nil {
		return nil, errors.New("batch store unavailable")
	}
	writer := p.env.DB()
	if conns := p.dbConns(); len(conns) > 0 {
		writer = conns[0]
	}
	if writer == nil {
		return nil, errors.New("batch store unavailable")
	}
	read := p.readDB()
	if read == nil {
		read = writer
	}
	return &BatchCLIStore{session.NewBatchStoreWithReader(writer, read), session.NewBatchLiveReader(db.New(read), p.env.DataDir())}, nil
}

func (a *App) BatchOwnerRuntime() bool {
	return batchOwnerRuntime(a.batchCLIParams())
}

func batchOwnerRuntime(p batchCLIParams) bool {
	return p.batchRuntime != nil && p.batchRuntime() != nil && p.batchInstall() != nil && p.batchInstall().Runner != nil
}

func (a *App) DataDirectory() string {
	return dataDirectory(a.batchCLIParams())
}

func dataDirectory(p batchCLIParams) string {
	if p.env.DataDir == nil {
		return ""
	}
	return p.env.DataDir()
}

func (a *App) BatchCLISession(ctx context.Context, id string) (session.Session, error) {
	return batchCLISession(a.batchCLIParams(), ctx, id)
}

func batchCLISession(p batchCLIParams, ctx context.Context, id string) (session.Session, error) {
	if id == "" {
		return p.env.Sessions().Create(ctx, "rush batch")
	}
	if p.env.Sessions().IsAgentToolSession(id) {
		return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", id)
	}
	sess, err := p.env.Sessions().Get(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	sess, err = validateResolvedSession(sess, id, p.env.WorkspaceRoot, p.env.ProcessHome())
	if err != nil {
		return session.Session{}, err
	}
	if p.env.Coord() != nil && p.env.Coord().IsSessionBusy(sess.ID) {
		return session.Session{}, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
	}
	if p.env.DataDir() == "" {
		return sess, nil
	}
	// Another process (rush run, web) holds the session's OS lock while it
	// drives a turn; probe it and release at once: the batch's own leaves
	// may need the lock.
	lock, err := session.TryAcquireSessionLock(p.env.DataDir(), sess.ID)
	if err != nil {
		var busyErr *session.SessionLockBusyError
		if errors.As(err, &busyErr) {
			return session.Session{}, fmt.Errorf("session %q is already processing another request (held by PID %d): %w", sess.ID, busyErr.HolderPID, agent.ErrSessionBusy)
		}
		return session.Session{}, fmt.Errorf("check session %q lock: %w", sess.ID, err)
	}
	if err := lock.Release(); err != nil {
		return session.Session{}, fmt.Errorf("release session %q lock probe: %w", sess.ID, err)
	}
	return sess, nil
}

func (a *App) BatchCLISpec(ctx context.Context, owner, messageID string, spec jobbatch.Spec) (jobbatch.Spec, func(), error) {
	return batchCLISpecWithQuestionRoute(a.batchCLIParams(), ctx, owner, messageID, spec, true)
}

func batchCLISpecWithQuestionRoute(p batchCLIParams, ctx context.Context, owner, messageID string, spec jobbatch.Spec, questionRoute bool) (jobbatch.Spec, func(), error) {
	if !batchOwnerRuntime(p) || p.env.Coord() == nil {
		return jobbatch.Spec{}, nil, errors.New("batch runtime unavailable")
	}
	admitted, err := agent.AdmitBatchRoleContracts(ctx, p.env.Coord(), spec)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	ctx = agent.WithAdmittedBatchRoleContracts(ctx)
	spec = admitted
	if err := persistSessionExecutionIntent(ctx, p.env.Sessions(), owner, sessionExecutionProviderOmitted, nil, false); err != nil {
		return jobbatch.Spec{}, nil, err
	}
	requestedDepth, depthErr := agent.MaxAgentDepthFrom(ctx)
	if depthErr != nil {
		return jobbatch.Spec{}, nil, depthErr
	}
	remainingDepth, depthErr := agent.PersistSessionAgentDepth(ctx, p.env.Sessions(), owner, requestedDepth)
	if depthErr != nil {
		return jobbatch.Spec{}, nil, depthErr
	}
	ctx, err = agent.WithAgentDepth(ctx, remainingDepth)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	b, err := agent.NewBatchCLIBuilderWithQuestionRoute(p.env.Coord(), p.batchWorkIngress(), questionRoute)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	return b.TranslateSpec(ctx, owner, messageID, spec)
}

func (a *App) BatchSubmit(ctx context.Context, owner string, spec jobbatch.Spec) (string, error) {
	return batchSubmitRefWithQuestionRoute(a.batchCLIParams(), ctx, owner, spec, nil, true)
}

// BatchAutoApproveOwner lets the tool calls of an API-owned batch service
// session run without a prompt. Only the "auto" token permission mode asks for
// it; the service session has no user to answer a prompt, and it is never a
// conversational session.
func (a *App) BatchAutoApproveOwner(owner string) {
	batchAutoApproveOwner(a.batchCLIParams(), owner)
}

func batchAutoApproveOwner(p batchCLIParams, owner string) {
	if p.env.Perms == nil || p.env.Perms() == nil {
		return
	}
	p.env.Perms().AutoApproveSession(owner)
}

func batchSubmitRefWithQuestionRoute(p batchCLIParams, ctx context.Context, owner string, spec jobbatch.Spec, ref func(root string) string, questionRoute bool) (string, error) {
	if !batchOwnerRuntime(p) {
		return "", errors.New("batch submit requires owning runtime")
	}
	if spec.Batch == nil || spec.Task != nil {
		return "", errors.New("batch submit requires batch root")
	}
	translated, release, err := batchCLISpecWithQuestionRoute(p, ctx, owner, uuid.NewString(), spec, questionRoute)
	if err != nil {
		return "", err
	}
	batch, err := p.batchInstall().SubmitCLIRef(ctx, owner, translated, ref)
	if err != nil {
		release()
		return "", err
	}
	if err := p.batchInstall().RetainCLIConstruction(batch.ID(), release); err != nil {
		release()
		return "", err
	}
	return string(batch.ID()), nil
}

// BatchSubmitRef is kept for the served intake and other route-ful callers.
func (a *App) BatchSubmitRef(ctx context.Context, owner string, spec jobbatch.Spec, ref func(root string) string) (string, error) {
	return batchSubmitRefWithQuestionRoute(a.batchCLIParams(), ctx, owner, spec, ref, true)
}

// BatchSubmitRefUnattended serves the standalone unattended CLI batch surface:
// no answering process is bound, so agent leaves must not get ask_question.
func (a *App) BatchSubmitRefUnattended(ctx context.Context, owner string, spec jobbatch.Spec) (string, error) {
	return batchSubmitRefWithQuestionRoute(a.batchCLIParams(), ctx, owner, spec, nil, false)
}

// BatchNodeOutput returns the live output of one node of a root this process
// still holds, or an error when it is released, expired or not started. It is
// bounded by ctx: the output of a node that is still running blocks until it
// ends, so callers pass only terminal nodes with a short deadline.
func (a *App) BatchNodeOutput(ctx context.Context, owner, root, node string) (string, error) {
	return batchNodeOutput(a.batchCLIParams(), ctx, owner, root, node)
}

func batchNodeOutput(p batchCLIParams, ctx context.Context, owner, root, node string) (string, error) {
	if p.batchInstall() == nil {
		return "", errors.New("batch runtime unavailable")
	}
	if p.batchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return "", errors.New("batch CLI: root is not owned by this session")
	}
	chunk, err := p.batchInstall().CLIOutput(ctx, owner, jobbatch.ID(root), jobbatch.ID(node))
	if err != nil {
		return "", err
	}
	return string(chunk.Data), nil
}

// BatchAnswerHeld answers the held question of a child of an owner session in
// the same batch execution (never as a new generation). The error is
// agent.ErrBatchHeldAnswerNone or a stale/stopped sentinel when there is no
// current question to answer.
func (a *App) BatchAnswerHeld(ctx context.Context, owner, childSessionID, text string) error {
	return batchAnswerHeld(a.batchCLIParams(), ctx, owner, childSessionID, text)
}

func batchAnswerHeld(p batchCLIParams, ctx context.Context, owner, childSessionID, text string) error {
	answerer, ok := p.env.Coord().(agent.BatchHeldAnswerer)
	if !ok {
		return errors.New("batch runtime unavailable")
	}
	_, err := answerer.AnswerBatchHeldQuestion(ctx, owner, childSessionID, text)
	return err
}

func batchSettleDebt(p batchCLIParams, ctx context.Context, owner, root string) error {
	if p.batchInstall() == nil || p.batchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return errors.New("batch CLI: root is not owned by this session")
	}
	fac, err := batchCLIStore(p)
	if err != nil {
		return err
	}
	return fac.Store.BatchSettleCLIWakeDebt(ctx, owner, root)
}

func (a *App) BatchRelease(ctx context.Context, owner, root string) error {
	return batchRelease(a.batchCLIParams(), ctx, owner, root)
}

func batchRelease(p batchCLIParams, ctx context.Context, owner, root string) error {
	if p.batchInstall() == nil || p.batchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return errors.New("batch CLI: root is not owned by this session")
	}
	fac, err := batchCLIStore(p)
	if err != nil {
		return err
	}
	if err = fac.Store.BatchSettleCLIWakeDebt(ctx, owner, root); err != nil {
		return err
	}
	return p.batchInstall().ReleaseCLIAfterResult(jobbatch.ID(root))
}

func (a *App) BatchResult(ctx context.Context, owner, root string) ([]db.BatchNode, map[string]string, error) {
	return batchResult(a.batchCLIParams(), ctx, owner, root)
}

func batchResult(p batchCLIParams, ctx context.Context, owner, root string) ([]db.BatchNode, map[string]string, error) {
	if p.batchInstall() == nil {
		return nil, nil, errors.New("batch runtime unavailable")
	}
	if p.batchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return nil, nil, errors.New("batch CLI: root is not owned by this session")
	}
	fac, err := batchCLIStore(p)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := fac.Store.Read(ctx, owner, root)
	if err != nil {
		return nil, nil, err
	}
	outputs := make(map[string]string, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if node.Kind != "task" {
			continue
		}
		chunk, outputErr := p.batchInstall().CLIOutput(ctx, owner, jobbatch.ID(root), jobbatch.ID(node.NodeID))
		if outputErr != nil {
			// Never started, or released and no longer cached: the outcome
			// still reports, without output.
			if errors.Is(outputErr, jobbatch.ErrNotStarted) || errors.Is(outputErr, agent.ErrBatchCLIOutputExpired) {
				continue
			}
			return nil, nil, fmt.Errorf("batch CLI output for %s: %w", node.NodeID, outputErr)
		}
		if len(chunk.Data) > 0 {
			outputs[node.NodeID] = string(chunk.Data)
		}
	}
	return snapshot.Nodes, outputs, nil
}

func batchWait(p batchCLIParams, ctx context.Context, owner, root string) (db.BatchNode, error) {
	if p.batchInstall() == nil {
		return db.BatchNode{}, errors.New("batch runtime unavailable")
	}
	if p.batchInstall().CLIOwner(jobbatch.ID(root)) != owner {
		return db.BatchNode{}, errors.New("batch CLI: root is not owned by this session")
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.batchInstall().WaitCLI(waitCtx, jobbatch.ID(root)); done <- err }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return db.BatchNode{}, err
			}
			goto completed
		case <-ctx.Done():
			return db.BatchNode{}, ctx.Err()
		case <-ticker.C:
			if err := p.batchInstall().PumpCLIControls(ctx, owner, jobbatch.ID(root)); err != nil {
				return db.BatchNode{}, err
			}
		}
	}
completed:
	fac, err := batchCLIStore(p)
	if err != nil {
		return db.BatchNode{}, err
	}
	snapshot, err := fac.Store.Read(ctx, owner, root)
	if err != nil {
		return db.BatchNode{}, err
	}
	return snapshot.Root, nil
}

func (a *App) BatchAdd(ctx context.Context, root, node string, items []jobbatch.Spec) error {
	return batchAdd(a.batchCLIParams(), ctx, root, node, items)
}

func batchAdd(p batchCLIParams, ctx context.Context, root, node string, items []jobbatch.Spec) error {
	if !batchOwnerRuntime(p) {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	fac, err := batchCLIStore(p)
	if err != nil {
		return err
	}
	rootIdentity, rootID, err := fac.Store.BatchCLIFind(ctx, root)
	if err != nil {
		return err
	}
	if rootID != rootIdentity.Root {
		return errors.New("first add argument must identify exact root")
	}
	nodeIdentity, nodeID, err := fac.Store.BatchCLIFind(ctx, node)
	if err != nil {
		return err
	}
	if nodeIdentity.Owner != rootIdentity.Owner || nodeIdentity.Root != rootIdentity.Root {
		return errors.New("node does not belong to selected root")
	}
	if hs := batchHostStore(p); hs == nil || hs.HostID() != rootIdentity.Host {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	if p.batchInstall().CLIOwner(jobbatch.ID(rootID)) != rootIdentity.Owner {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	// CLI owner-process add = the same unattended surface.
	b, err := agent.NewBatchCLIBuilderWithQuestionRoute(p.env.Coord(), p.batchWorkIngress(), false)
	if err != nil {
		return err
	}
	owner := rootIdentity.Owner
	translated, release, err := b.TranslateItems(ctx, owner, uuid.NewString(), items)
	if err != nil {
		return err
	}
	if err := p.batchInstall().AddCLI(ctx, jobbatch.ID(rootID), jobbatch.ID(nodeID), translated); err != nil {
		release()
		return err
	}
	if err := p.batchInstall().RetainCLIConstruction(jobbatch.ID(rootID), release); err != nil {
		release()
		return err
	}
	return nil
}
