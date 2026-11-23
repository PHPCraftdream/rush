// session_bridge.go consolidates the root-package facade re-exports that used
// to live in five separate files (batch read, finalization debt, finalization
// facts, finalization holds, notice/host aliases). Declarations are unchanged:
// external packages keep referring to them as session.<Name>.

package session

import (
	"context"
	"database/sql"
	"time"

	findebt "github.com/PHPCraftdream/rush/internal/session/batch/findebt"
	finfacts "github.com/PHPCraftdream/rush/internal/session/batch/finfacts"
	finholds "github.com/PHPCraftdream/rush/internal/session/batch/finholds"
	finterm "github.com/PHPCraftdream/rush/internal/session/batch/finterm"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatch"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatchread"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	sessionfacts "github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

type (
	BatchLiveSource     = sessionbatchread.BatchLiveSource
	BatchLiveReader     = sessionbatchread.BatchLiveReader
	BatchLiveTxStarter  = sessionbatchread.BatchLiveTxStarter
	BatchLiveRef        = sessionbatchread.BatchLiveRef
	BatchLiveCounts     = sessionbatchread.BatchLiveCounts
	BatchLiveNode       = sessionbatchread.BatchLiveNode
	BatchLiveResult     = sessionbatchread.BatchLiveResult
	BatchLevelResult    = sessionbatchread.BatchLevelResult
	BatchCapturedRoots  = sessionbatchread.BatchCapturedRoots
	BatchCapturedLevels = sessionbatchread.BatchCapturedLevels
	BatchTimeoutCheckin = sessionbatchread.BatchTimeoutCheckin
)

const (
	DefaultBatchTimeoutCheckinPageSize   = sessionbatchread.DefaultBatchTimeoutCheckinPageSize
	DefaultBatchTimeoutCheckinScanBudget = sessionbatchread.DefaultBatchTimeoutCheckinScanBudget
	DefaultBatchTimeoutCheckinInterval   = sessionbatchread.DefaultBatchTimeoutCheckinInterval
)

var errBatchReadUnavailable = sessionbatchread.ErrBatchReadUnavailable

func NewBatchLiveReader(source BatchLiveSource, dataDir string) *BatchLiveReader {
	return sessionbatchread.NewBatchLiveReader(source, dataDir)
}

func NewBatchTimeoutCheckin(sqlDB *sql.DB) *BatchTimeoutCheckin {
	return sessionbatchread.NewBatchTimeoutCheckin(sqlDB)
}

func SetBatchLiveServedSeamForTest(hook func()) (restore func()) {
	return sessionbatchread.SetBatchLiveServedSeamForTest(hook)
}

type (
	BatchStore              = sessionbatch.BatchStore
	BatchReservation        = sessionbatch.BatchReservation
	BatchReservationNode    = sessionbatch.BatchReservationNode
	BatchReservationResult  = sessionbatch.BatchReservationResult
	BatchSnapshot           = sessionbatch.BatchSnapshot
	BatchAddition           = sessionbatch.BatchAddition
	BatchProjection         = sessionbatch.BatchProjection
	BatchRootProjection     = sessionbatch.BatchRootProjection
	BatchLiveProbe          = sessionbatch.BatchLiveProbe
	BatchAPIRoot            = sessionbatch.BatchAPIRoot
	BatchAPIHeldQuestion    = sessionbatch.BatchAPIHeldQuestion
	BatchCLIIdentity        = sessionbatch.BatchCLIIdentity
	BatchControlTarget      = sessionbatch.BatchControlTarget
	BatchControlAccepted    = sessionbatch.BatchControlAccepted
	BatchControlCommit      = sessionbatch.BatchControlCommit
	BatchControlPump        = sessionbatch.BatchControlPump
	BatchInlineAnnouncement = sessionbatch.BatchInlineAnnouncement
	BatchToolResultIdentity = sessionbatch.BatchToolResultIdentity
	BatchRerunVoidedNode    = sessionbatch.BatchRerunVoidedNode
	RerunTruncateParams     = sessionbatch.RerunTruncateParams
	BatchRerunTruncation    = sessionbatch.BatchRerunTruncation
	BatchResetOutcome       = sessionbatch.BatchResetOutcome
	BatchHistorySnapshot    = sessionbatch.BatchHistorySnapshot
	BatchHistoryNodeView    = sessionbatch.BatchHistoryNodeView
	BatchResetRefusal       = sessionbatch.BatchResetRefusal
	BatchOwnerDeleteOutcome = sessionbatch.BatchOwnerDeleteOutcome
	BatchNoticeRow          = sessionbatch.BatchNoticeRow
	BatchPulledNotice       = sessionbatch.BatchPulledNotice
	BatchDebtNodeRef        = sessionbatch.BatchDebtNodeRef
	BatchDebtSnapshot       = sessionbatch.BatchDebtSnapshot
	BatchPendingDebt        = sessionbatch.BatchPendingDebt
	BatchReactedFailedText  = sessionbatch.BatchReactedFailedText
	BatchHostProbe          = sessionbatch.BatchHostProbe
	BatchRecoveryOutcome    = sessionbatch.BatchRecoveryOutcome
	BatchRecoveryCursor     = sessionbatch.BatchRecoveryCursor
	BatchRecoveryLimits     = sessionbatch.BatchRecoveryLimits
	BatchStopOwnerTarget    = sessionbatch.BatchStopOwnerTarget
)

const (
	AsyncDataRetentionAge    = sessionbatch.AsyncDataRetentionAge
	BatchRecoveryCursorStart = sessionbatch.BatchRecoveryCursorStart
)

var (
	ErrBatchAnnouncementLost    = sessionbatch.ErrBatchAnnouncementLost
	ErrBatchToolResultIdentity  = sessionbatch.ErrBatchToolResultIdentity
	ErrRerunTargetGone          = sessionbatch.ErrRerunTargetGone
	ErrBatchResetRefused        = sessionbatch.ErrBatchResetRefused
	ErrBatchValueOverflow       = sessionbatch.ErrBatchValueOverflow
	ErrBatchBindNodeUnknown     = sessionbatch.ErrBatchBindNodeUnknown
	ErrBatchBindForeignIdentity = sessionbatch.ErrBatchBindForeignIdentity
	ErrBatchBindStaleToken      = sessionbatch.ErrBatchBindStaleToken
	ErrBatchBindNodeKind        = sessionbatch.ErrBatchBindNodeKind
	ErrBatchBindNodeNotLive     = sessionbatch.ErrBatchBindNodeNotLive
	ErrBatchBindRootNotLive     = sessionbatch.ErrBatchBindRootNotLive
	ErrBatchBindChildConflict   = sessionbatch.ErrBatchBindChildConflict
	ErrBatchBindChildForeign    = sessionbatch.ErrBatchBindChildForeign
	ErrBatchBindLostUpdate      = sessionbatch.ErrBatchBindLostUpdate
	ErrBatchInputMismatch       = sessionbatch.ErrBatchInputMismatch
	ErrBatchForeignLiveHost     = sessionbatch.ErrBatchForeignLiveHost
	ErrBatchRunnerMismatch      = sessionbatch.ErrBatchRunnerMismatch
	ErrBatchControlStale        = sessionbatch.ErrBatchControlStale
	ErrBatchControlHostMismatch = sessionbatch.ErrBatchControlHostMismatch
	ErrBatchControlTerminal     = sessionbatch.ErrBatchControlTerminal
	ErrBatchControlUnknownHost  = sessionbatch.ErrBatchControlUnknownHost
)

func NewBatchStore(writer *sql.DB) *BatchStore {
	return sessionbatch.NewBatchStore(writer)
}

func NewBatchStoreWithReader(writer, reader *sql.DB) *BatchStore {
	return sessionbatch.NewBatchStoreWithReader(writer, reader)
}

func NewBatchControlPump(s *BatchStore, owningHost string, commit BatchControlCommit) *BatchControlPump {
	return sessionbatch.NewBatchControlPump(s, owningHost, commit)
}

func NewBatchCommitHook(store *BatchStore, id session.BatchCommitIdentity) func(context.Context, jobbatch.Transition) error {
	return sessionbatch.NewBatchCommitHook(store, id)
}

func BatchSharedHostProbe(dataDir string) BatchHostProbe {
	return sessionbatch.BatchSharedHostProbe(dataDir)
}

func BatchAPIRootLive(state string) bool {
	return sessionbatch.BatchAPIRootLive(state)
}

func BatchRetentionCutoff(now time.Time) int64 {
	return sessionbatch.BatchRetentionCutoff(now)
}

func BatchRerunVoidedRoots(res BatchRerunTruncation) []BatchRerunVoidedNode {
	return sessionbatch.BatchRerunVoidedRoots(res)
}

type FinalizationDebtStore = findebt.FinalizationDebtStore

type AgentFinalizationDebtSettler = findebt.AgentFinalizationDebtSettler

func NewFinalizationDebtStore(sqlDB *sql.DB) *FinalizationDebtStore {
	return findebt.NewFinalizationDebtStore(sqlDB)
}

type FinalizationDescendantsFacts = finfacts.FinalizationDescendantsFacts

type FinalizationQueuedCallsFacts = finfacts.FinalizationQueuedCallsFacts

type FinalizationPhysicalHoldsFacts = finfacts.FinalizationPhysicalHoldsFacts

type FinalizationReactionDebtFacts = finfacts.FinalizationReactionDebtFacts

type FinalizationDescendantsReader = finfacts.FinalizationDescendantsReader

func NewFinalizationDescendantsReader() FinalizationDescendantsReader {
	return finfacts.NewFinalizationDescendantsReader()
}

func NewFinalizationQueuedCallsFacts() FinalizationQueuedCallsFacts {
	return finfacts.NewFinalizationQueuedCallsFacts()
}

func NewAgentFinalizationScopeReader(
	ownerSessionID string,
	descendants FinalizationDescendantsFacts,
	queued FinalizationQueuedCallsFacts,
	holds FinalizationPhysicalHoldsFacts,
	debt FinalizationReactionDebtFacts,
) finterm.AgentFinalizationScopeReader {
	return finfacts.NewAgentFinalizationScopeReader(ownerSessionID, descendants, queued, holds, debt)
}

var ErrFinalizationPhysicalHoldsUnreadable = finholds.ErrFinalizationPhysicalHoldsUnreadable

func NewFinalizationPhysicalHoldsFacts() FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationPhysicalHoldsFacts()
}

func NewFinalizationHostDriverHoldsFacts(dataDir string) FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationHostDriverHoldsFacts(dataDir)
}

type FinalizationHoldsProvider = finholds.FinalizationHoldsProvider

func NewFinalizationPhysicalHoldsFactsFromProviders(providers ...FinalizationHoldsProvider) FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationPhysicalHoldsFactsFromProviders(providers...)
}

func NewFinalizationPhysicalHoldsFactsSum(a, b FinalizationPhysicalHoldsFacts) FinalizationPhysicalHoldsFacts {
	return finholds.NewFinalizationPhysicalHoldsFactsSum(a, b)
}

func CountFinalizationTrackedProcessHolds() int {
	return finholds.CountFinalizationTrackedProcessHolds()
}

// Notice-kind vocabulary (moved to sessionnotice / sessionwake).
const (
	NoticeKindWakeOnly         = sessionnotice.NoticeKindWakeOnly
	NoticeKindSupervision      = sessionnotice.NoticeKindSupervision
	NoticeKindWakeFailed       = sessionnotice.NoticeKindWakeFailed
	NoticeKindBGShellDone      = sessionnotice.NoticeKindBGShellDone
	NoticeKindChildQuestion    = sessionnotice.NoticeKindChildQuestion
	NoticeKindReactionChain    = sessionnotice.NoticeKindReactionChain
	NoticeKindWakeFired        = sessionwake.NoticeKindWakeFired
	AwaitingAnswerStoppedTitle = sessionnotice.AwaitingAnswerStoppedTitle
)

// Host probe statuses (moved to sessionhosts).
const (
	HostStatusUnknown = sessionhosts.HostStatusUnknown
	HostStatusAlive   = sessionhosts.HostStatusAlive
	HostStatusDead    = sessionhosts.HostStatusDead
)

// WakeLeaseTTL is the moved sessionwake claim-lease TTL.
const WakeLeaseTTL = sessionwake.WakeLeaseTTL

// Host-store / probe helpers and types (moved to sessionhosts).
type (
	HostStore = sessionhosts.HostStore
)

var ProbeHostLock = sessionhosts.ProbeHostLock

// Wake schedule store type (moved to sessionwake).
type WakeScheduleStore = sessionwake.WakeScheduleStore

// WaitSchedule is the moved sessionfacts wait-reason constant.
const WaitSchedule = sessionfacts.WaitSchedule

// Session activity / lock-fact kinds (moved to sessionfacts).
const (
	ActivityIdle         = sessionfacts.ActivityIdle
	ActivityInTurn       = sessionfacts.ActivityInTurn
	ActivityBetweenTurns = sessionfacts.ActivityBetweenTurns
	ActivityEnded        = sessionfacts.ActivityEnded
	ActivityCrashed      = sessionfacts.ActivityCrashed
	ActivityDelegating   = sessionfacts.ActivityDelegating
	LockUnknown          = sessionfacts.LockUnknown
	LockHeld             = sessionfacts.LockHeld
	LockReleased         = sessionfacts.LockReleased
	LockDead             = sessionfacts.LockDead
)

// HostLockStatus keeps its root-package name for external callers.
type HostLockStatus = sessionhosts.HostLockStatus
