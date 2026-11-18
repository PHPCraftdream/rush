package session

// Re-exports of notice/host constants that moved into the internal/session/state
// sub-packages (lane MV5). External packages (internal/agent, cmd/...) still
// refer to them as session.<Name>; the values are identical to HEAD~.

import (
	sessionfacts "github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

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
