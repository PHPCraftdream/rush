package session

import (
	"context"
	"database/sql"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatch"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatchread"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
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
