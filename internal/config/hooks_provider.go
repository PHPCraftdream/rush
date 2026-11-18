package config

import (
	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
)

// init registers the root hook provider so lower IO packages observe the
// root test seams at the same moment the old direct reads did.
func init() {
	cfghooks.SetProvider(func() cfghooks.Hooks {
		configTestHooks.Lock()
		defer configTestHooks.Unlock()
		return cfghooks.Hooks{
			BeforeOpen:            configTestHooks.beforeOpen,
			AfterStableRead:       configTestHooks.afterStableRead,
			BeforeCommitCheck:     configTestHooks.beforeCommitCheck,
			AfterCommitRename:     configTestHooks.afterCommitRename,
			AfterCommitRenamePath: configTestHooks.afterCommitRenamePath,
			AfterMCPCommit:        configTestHooks.afterMCPCommit,
			BeforeCommitRename:    configTestHooks.beforeCommitRename,
			BeforeMCPReconcile:    configTestHooks.beforeMCPReconcile,
			AcquireConfigLock:     configTestHooks.acquireConfigLock,
			ReleaseConfigLock:     configTestHooks.releaseConfigLock,
			WithConfigTimeout:     configTestHooks.withConfigTimeout,
			ForceLinkNoReplace:    configTestHooks.forceLinkNoReplace,
			RenameNoReplace:       configTestHooks.renameNoReplace,
			RenameatxNp:           configTestHooks.renameatxNp,
			MoveFileEx:            configTestHooks.moveFileEx,
			SetFileInformation:    configTestHooks.setFileInformation,
			FlushFileBuffers:      configTestHooks.flushFileBuffers,
			UnlinkTemp:            configTestHooks.unlinkTemp,
			SyncParent:            configTestHooks.syncParent,
			SyncParentFD:          configTestHooks.syncParentFD,
			SyncParentFile:        configTestHooks.syncParentFile,
		}
	})
}
