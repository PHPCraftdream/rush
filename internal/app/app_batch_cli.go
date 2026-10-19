package app

import (
	"context"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

type BatchCLIStore struct {
	Store  *session.BatchStore
	Reader *session.BatchLiveReader
}

func (a *App) BatchCLIStore() (*BatchCLIStore, error) {
	if a == nil || a.DB == nil {
		return nil, errors.New("batch store unavailable")
	}
	writer := a.DB()
	if len(a.dbConns) > 0 {
		writer = a.dbConns[0]
	}
	if writer == nil {
		return nil, errors.New("batch store unavailable")
	}
	read := a.readDB
	if read == nil {
		read = writer
	}
	return &BatchCLIStore{session.NewBatchStore(writer), session.NewBatchLiveReader(db.New(read), a.dataDir)}, nil
}

func (a *App) BatchOwnerRuntime() bool {
	return a != nil && a.batchRuntime != nil && a.batchInstall != nil && a.batchInstall.Runner != nil && a.asyncJobStore != nil
}

func (a *App) DataDirectory() string {
	if a == nil {
		return ""
	}
	return a.dataDir
}

func (a *App) BatchCLISession(ctx context.Context, id string) (session.Session, error) {
	if id != "" {
		return a.Sessions.Get(ctx, id)
	}
	return a.Sessions.Create(ctx, "rush batch")
}

func (a *App) BatchCLISpec(ctx context.Context, owner, messageID string, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if !a.BatchOwnerRuntime() || a.AgentCoordinator == nil {
		return jobbatch.Spec{}, errors.New("batch runtime unavailable")
	}
	b, err := agent.NewBatchCLIBuilder(a.AgentCoordinator, a.batchWorkIngress)
	if err != nil {
		return jobbatch.Spec{}, err
	}
	return b.TranslateSpec(ctx, owner, messageID, spec)
}

func (a *App) BatchSubmit(ctx context.Context, owner string, spec jobbatch.Spec) (string, error) {
	if !a.BatchOwnerRuntime() {
		return "", errors.New("batch submit requires owning runtime")
	}
	if spec.Batch == nil || spec.Task != nil {
		return "", errors.New("batch submit requires batch root")
	}
	translated, err := a.BatchCLISpec(ctx, owner, uuid.NewString(), spec)
	if err != nil {
		return "", err
	}
	batch, err := a.batchInstall.SubmitCLI(ctx, owner, translated)
	if err != nil {
		return "", err
	}
	return string(batch.ID()), nil
}

func (a *App) BatchWait(ctx context.Context, owner, root string) (db.BatchNode, error) {
	if a == nil || a.batchInstall == nil {
		return db.BatchNode{}, errors.New("batch runtime unavailable")
	}
	if a.batchInstall.CLIOwner(jobbatch.ID(root)) != owner {
		return db.BatchNode{}, errors.New("batch CLI: root is not owned by this session")
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.batchInstall.WaitCLI(waitCtx, jobbatch.ID(root)); done <- err }()
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
			if err := a.batchInstall.PumpCLIControls(ctx, owner, jobbatch.ID(root)); err != nil {
				return db.BatchNode{}, err
			}
		}
	}
completed:
	fac, err := a.BatchCLIStore()
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
	if !a.BatchOwnerRuntime() {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	fac, err := a.BatchCLIStore()
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
	if a.asyncJobStore.HostID() != rootIdentity.Host {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	if a.batchInstall.CLIOwner(jobbatch.ID(rootID)) != rootIdentity.Owner {
		return errors.New("batch add is owner-process-only; use rush batch stop for cross-process control")
	}
	b, err := agent.NewBatchCLIBuilder(a.AgentCoordinator, a.batchWorkIngress)
	if err != nil {
		return err
	}
	owner := rootIdentity.Owner
	translated, err := b.TranslateItems(ctx, owner, uuid.NewString(), items)
	if err != nil {
		return err
	}
	return a.batchInstall.AddCLI(ctx, jobbatch.ID(rootID), jobbatch.ID(nodeID), translated)
}
