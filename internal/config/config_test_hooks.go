package config

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
)

// configTestHooks is the root package's original test seam registry. It keeps
// the exact original fields (including the root-only MCP resolution seams)
// and is bridged into io/cfghooks by the init in hooks_provider.go so lower
// IO packages observe the root seams unchanged.
var configTestHooks struct {
	sync.Mutex
	beforeOpen            func(string)
	afterStableRead       func(string)
	beforeCommitCheck     func()
	afterCommitRename     func() error
	afterCommitRenamePath func(string) error
	// Test-only seam after a successful MCP commit.
	afterMCPCommit func(string)
	// Test-only seam between target resolution and sidecar acquisition.
	// Root-only: root production calls it on the root configWriteTarget.
	afterMCPResolveTarget func(*configWriteTarget)
	// Test-only seam before MCP target binding verification.
	// Root-only: root production calls it on the root configWriteTarget.
	beforeMCPTargetBindingVerification func(*configWriteTarget)
	beforeCommitRename                 func()
	beforeMCPReconcile                 func(string, []byte)
	acquireConfigLock                  func(context.Context, string) (*filelock.FileLock, error)
	releaseConfigLock                  func(string, *filelock.FileLock) error
	withConfigTimeout                  func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	forceLinkNoReplace                 bool
	renameNoReplace                    func(int, string, int, string) error
	renameatxNp                        func(int, string, int, string, uint32) error
	moveFileEx                         func(*uint16, *uint16, uint32) error
	setFileInformation                 func(uintptr, uint32, *byte, uint32) error
	flushFileBuffers                   func(uintptr) error
	unlinkTemp                         func(int, string) error
	syncParent                         func(string) error
	syncParentFD                       func(int) error
	syncParentFile                     func(*os.File) error
}

// runConfigAfterMCPResolveTargetHook is a root-only seam: the root
// production MCP admission path calls it with the root configWriteTarget.
func runConfigAfterMCPResolveTargetHook(target *configWriteTarget) {
	configTestHooks.Lock()
	hook := configTestHooks.afterMCPResolveTarget
	configTestHooks.Unlock()
	if hook != nil {
		hook(target)
	}
}

// runConfigBeforeMCPTargetBindingVerificationHook is a root-only seam: the
// root production write path calls it with the root configWriteTarget.
func runConfigBeforeMCPTargetBindingVerificationHook(target *configWriteTarget) {
	configTestHooks.Lock()
	hook := configTestHooks.beforeMCPTargetBindingVerification
	configTestHooks.Unlock()
	if hook != nil {
		hook(target)
	}
}
