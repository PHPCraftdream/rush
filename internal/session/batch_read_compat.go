package session

import (
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatchread"
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
