package agent

import (
	"errors"
	"fmt"
	"sort"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Batch group names. The set is closed; "" is the explicit default group.
const (
	batchGroupIO          = "io"
	batchGroupCLIDriver   = "cli_driver"
	batchGroupAgentDriver = "agent_driver"
	batchGroupDefault     = "" // The explicit default group.
)

// Batch group ranks. Lower ranks must never wait on equal or higher ranks.
// batchGroupDefault shares rank 0 with io: default-group work is plain
// non-delegating work, so it shares rank 0; it is still a distinct capped
// group.
var batchGroupRanks = map[string]int{
	batchGroupIO:          0,
	batchGroupCLIDriver:   1,
	batchGroupAgentDriver: 2,
	batchGroupDefault:     0,
}

// Typed constructor rejections. All are sentinel errors distinguishable via
// errors.Is.
var (
	errBatchCapInvalid       = errors.New("batch profile cap must be > 0")
	errBatchGlobalTooSmall   = errors.New("batch profile global cap below sum of group caps")
	errBatchMissingGroup     = errors.New("batch profile missing a required group cap")
	errBatchUnknownGroupName = errors.New("batch profile unknown group name")
	errBatchRankConflict     = errors.New("batch profile duplicate rank in rank table")
)

// BatchProfileCaps are the per-group and global caps of a profile. All group
// caps must be > 0 and Global must be >= their sum.
type BatchProfileCaps struct {
	IO          int
	CLIDriver   int
	AgentDriver int
	Default     int
	Global      int
}

// BatchProfile is an immutable closed capacity profile. It validates at
// construction and exposes only lookups; there are no setters and no exported
// mutable state.
type BatchProfile struct {
	caps   map[string]int
	ranks  map[string]int
	global int
}

// validateBatchRanks builds the fixed rank table and rejects duplicate ranks.
// The documented exception is the shared rank 0 of io and the default group:
// default-group work is plain non-delegating work, so it shares rank 0 with
// io while remaining a distinct capped group.
func validateBatchRanks(ranks map[string]int) error {
	sharedRank0 := map[string]bool{batchGroupIO: true, batchGroupDefault: true}
	seen := make(map[int]string, len(ranks))
	for group, rank := range ranks {
		if prev, ok := seen[rank]; ok {
			allow := rank == 0 && sharedRank0[prev] && sharedRank0[group]
			if !allow {
				return fmt.Errorf("%w: %q and %q both rank %d", errBatchRankConflict, prev, group, rank)
			}
			continue
		}
		seen[rank] = group
	}
	return nil
}

// NewBatchProfile validates caps and returns an immutable profile.
func NewBatchProfile(caps BatchProfileCaps) (*BatchProfile, error) {
	groups := map[string]int{
		batchGroupIO:          caps.IO,
		batchGroupCLIDriver:   caps.CLIDriver,
		batchGroupAgentDriver: caps.AgentDriver,
		batchGroupDefault:     caps.Default,
	}
	return newBatchProfileFromGroups(groups, caps.Global)
}

// NewBatchProfileFromMap validates a map-form profile. It rejects unknown
// group names, missing required groups (including "") and any cap <= 0.
func NewBatchProfileFromMap(groups map[string]int, global int) (*BatchProfile, error) {
	caps := make(map[string]int, len(groups))
	for name, cap := range groups {
		switch name {
		case batchGroupIO, batchGroupCLIDriver, batchGroupAgentDriver, batchGroupDefault:
			caps[name] = cap
		default:
			return nil, fmt.Errorf("%w: %q", errBatchUnknownGroupName, name)
		}
	}
	for _, name := range batchProfileGroups() {
		if _, ok := caps[name]; !ok {
			return nil, fmt.Errorf("%w: %q", errBatchMissingGroup, name)
		}
	}
	return newBatchProfileFromGroups(caps, global)
}

// newBatchProfileFromGroups applies the shared cap, sum and rank validation.
func newBatchProfileFromGroups(caps map[string]int, global int) (*BatchProfile, error) {
	if global <= 0 {
		return nil, fmt.Errorf("%w: global cap %d", errBatchCapInvalid, global)
	}
	sum := 0
	for name, cap := range caps {
		if cap <= 0 {
			return nil, fmt.Errorf("%w: group %q cap %d", errBatchCapInvalid, name, cap)
		}
		sum += cap
	}
	if global < sum {
		return nil, fmt.Errorf("%w: global %d < sum %d", errBatchGlobalTooSmall, global, sum)
	}
	ranks := make(map[string]int, len(batchGroupRanks))
	for name, rank := range batchGroupRanks {
		if _, ok := caps[name]; !ok {
			return nil, fmt.Errorf("%w: %q", errBatchMissingGroup, name)
		}
		ranks[name] = rank
	}
	if err := validateBatchRanks(ranks); err != nil {
		return nil, err
	}
	capsCopy := make(map[string]int, len(caps))
	for name, cap := range caps {
		capsCopy[name] = cap
	}
	return &BatchProfile{caps: capsCopy, ranks: ranks, global: global}, nil
}

// NewBatchProfileDefault returns the documented default profile.
//
// IO = 50 anchors the existing background-shell cap MaxBackgroundJobs = 50 in
// internal/shell/background.go. CLIDriver = 1, AgentDriver = 1 and Default = 1
// have no existing code anchor: they are chosen minimal, i.e. strictly serial
// rank>0 delegation and minimal default-group width, not derived from code.
// Global = 53 is the sum of the four caps, the minimum legal global cap.
func NewBatchProfileDefault() (*BatchProfile, error) {
	return NewBatchProfile(BatchProfileCaps{IO: 50, CLIDriver: 1, AgentDriver: 1, Default: 1, Global: 53})
}

// Limits maps the profile onto engine limits. The ByGroup map is a fresh copy;
// mutating it never affects the profile.
func (p *BatchProfile) Limits(maxDepth, maxNodes int) jobbatch.Limits {
	byGroup := make(map[string]int, len(p.caps))
	for name, cap := range p.caps {
		byGroup[name] = cap
	}
	return jobbatch.Limits{MaxConcurrent: p.global, ByGroup: byGroup, MaxDepth: maxDepth, MaxNodes: maxNodes}
}

// GroupCap returns the cap of a group and whether the group is in the profile.
func (p *BatchProfile) GroupCap(group string) (int, bool) {
	cap, ok := p.caps[group]
	return cap, ok
}

// GroupRank returns the rank of a group, including the default group "".
func (p *BatchProfile) GroupRank(group string) (int, bool) {
	rank, ok := p.ranks[group]
	return rank, ok
}

// Groups returns the closed group set in sorted order.
func (p *BatchProfile) Groups() []string {
	return batchProfileGroups()
}

// batchProfileGroups returns a fresh sorted copy of the closed group set.
func batchProfileGroups() []string {
	groups := make([]string, 0, len(batchGroupRanks))
	for name := range batchGroupRanks {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	return groups
}
