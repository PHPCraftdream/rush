package cfghooks

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
)

type Hooks struct {
	BeforeOpen            func(string)
	AfterStableRead       func(string)
	BeforeCommitCheck     func()
	AfterCommitRename     func() error
	AfterCommitRenamePath func(string) error
	AfterMCPCommit        func(string)
	BeforeCommitRename    func()
	BeforeMCPReconcile    func(string, []byte)
	AcquireConfigLock     func(context.Context, string) (*filelock.FileLock, error)
	ReleaseConfigLock     func(string, *filelock.FileLock) error
	WithConfigTimeout     func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	ForceLinkNoReplace    bool
	RenameNoReplace       func(int, string, int, string) error
	RenameatxNp           func(int, string, int, string, uint32) error
	MoveFileEx            func(*uint16, *uint16, uint32) error
	SetFileInformation    func(uintptr, uint32, *byte) error
	FlushFileBuffers      func(uintptr) error
	UnlinkTemp            func(int, string) error
	SyncParent            func(string) error
	SyncParentFD          func(int) error
	SyncParentFile        func(*os.File) error
}

var (
	mu       sync.Mutex
	provider func() Hooks
)

// SetProvider stores the root package's hook provider.
func SetProvider(p func() Hooks) {
	mu.Lock()
	provider = p
	mu.Unlock()
}

// Snapshot calls the provider outside the registry lock. The provider is
// registered once by the root package so lower packages observe the root test
// seams at the same moment the old direct reads did; Snapshot copies under the
// provider's own synchronization.
func Snapshot() Hooks {
	mu.Lock()
	p := provider
	mu.Unlock()
	if p == nil {
		return Hooks{}
	}
	return p()
}
