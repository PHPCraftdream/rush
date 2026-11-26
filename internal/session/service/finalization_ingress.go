package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	finterm "github.com/PHPCraftdream/rush/internal/session/batch/finterm"
	"github.com/PHPCraftdream/rush/internal/session/service/components/sessguard"
	"github.com/PHPCraftdream/rush/internal/session/service/components/sesstypes"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

type finalizationIngressPayload struct {
	data        SessionAgentCallData
	bound       bool
	identities  string
	payloadHash string
}

func (s *service) prepareStoredFinalizationIngress(owner string, raw []byte) (finalizationIngressPayload, error) {
	var payload finalizationIngressPayload
	err := json.Unmarshal(raw, &payload.data)
	payload.bound = payload.data.FinalizationRequirement != nil
	if !payload.bound {
		return payload, nil
	}
	if err != nil || payload.data.SessionID != owner || payload.data.LogicalCallID == "" {
		return payload, ErrAgentFinalizationChanged
	}
	payload.identities, err = CanonicalFinalizationRequirement(payload.data.FinalizationRequirement)
	if err != nil {
		return payload, err
	}
	payload.payloadHash, err = FinalizationPayloadHash(payload.data)
	return payload, err
}

func (s *service) prepareFinalizationIngress(ctx context.Context, owner string, raw []byte) (finalizationIngressPayload, error) {
	var payload finalizationIngressPayload
	err := json.Unmarshal(raw, &payload.data)
	payload.bound = payload.data.FinalizationRequirement != nil
	if !payload.bound {
		if len(CaptureAgentFinalizationContext(ctx).authorities) != 0 {
			return payload, ErrAgentFinalizationChanged
		}
		return payload, nil
	}
	if err != nil || payload.data.SessionID != owner || payload.data.LogicalCallID == "" {
		return payload, ErrAgentFinalizationChanged
	}
	payload.identities, err = trustedFinalizationDeclaration(ctx, s, payload.data)
	if err != nil {
		return payload, err
	}
	payload.payloadHash, err = FinalizationPayloadHash(payload.data)
	return payload, err
}

func validateFreshFinalizationIngressTx(ctx context.Context, tx *sql.Tx, owner, id string, payload finalizationIngressPayload, ancestryHash string, admission *BatchFinalizationAdmission) error {
	if err := ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return err
	}
	if exists, err := finalizationContinuationExistsTx(ctx, tx, id); err != nil {
		return err
	} else if exists {
		return ErrAgentFinalizationChanged
	}
	if !payload.bound {
		return nil
	}
	ancestry, err := ReadFinalizationAncestry(ctx, tx, []string{owner})
	if err != nil {
		return err
	}
	if FinalizationAncestryHash(ancestry) != ancestryHash {
		return ErrAgentFinalizationChanged
	}
	for _, identity := range payload.data.FinalizationRequirement.Authorities {
		if _, affected := searchFinalizationOwner(ancestry.owners, identity.SessionID); !affected {
			return ErrAgentFinalizationChanged
		}
	}
	return nil
}

type finalizationIngressQueryRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func finalizationQueueRowTx(ctx context.Context, tx finalizationIngressQueryRow, id string) (owner, raw string, exists bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT session_id, call_data FROM session_run_queue WHERE id=?`, id).Scan(&owner, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return owner, raw, err == nil, err
}

func finalizationOutboxExistsTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM orphan_call_outbox WHERE id=?)`, id).Scan(&exists)
	return exists, err
}

func validateFinalizationIngressTx(ctx context.Context, tx *sql.Tx, owner, id, raw string, payload finalizationIngressPayload) (bool, error) {
	var storedOwner, logical, payloadHash, identities, ancestryHash string
	var consumed int64
	err := tx.QueryRowContext(ctx, `SELECT session_id, logical_call_id, payload_hash, identities, ancestry, consumed
		FROM session_agent_finalization_continuations WHERE row_id=?`, id).Scan(&storedOwner, &logical, &payloadHash, &identities, &ancestryHash, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		if payload.bound {
			return false, ErrAgentFinalizationChanged
		}
		if err := ValidateBatchFinalizationTx(ctx, tx, []string{owner}, nil); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !payload.bound || storedOwner != owner || logical != payload.data.LogicalCallID || payloadHash != payload.payloadHash || identities != payload.identities || consumed != 0 {
		return false, ErrAgentFinalizationChanged
	}
	if raw != "" {
		var actual SessionAgentCallData
		if err := json.Unmarshal([]byte(raw), &actual); err != nil {
			return false, err
		}
		actualHash, err := FinalizationPayloadHash(actual)
		if err != nil {
			return false, err
		}
		declaration, err := CanonicalFinalizationRequirement(actual.FinalizationRequirement)
		if err != nil || actual.SessionID != owner || actual.LogicalCallID != logical || actualHash != payloadHash || declaration != identities {
			return false, ErrAgentFinalizationChanged
		}
	}
	declaration, err := CanonicalFinalizationRequirement(payload.data.FinalizationRequirement)
	if err != nil || declaration != identities {
		return false, ErrAgentFinalizationChanged
	}
	ancestry, err := ReadFinalizationAncestry(ctx, tx, []string{owner})
	if err != nil {
		return false, err
	}
	if FinalizationAncestryHash(ancestry) != ancestryHash {
		return false, ErrAgentFinalizationChanged
	}
	var storedRequirement AgentFinalizationRequirement
	if err := json.Unmarshal([]byte(identities), &storedRequirement); err != nil {
		return false, err
	}
	byOwner := make(map[string]bool, len(storedRequirement.Authorities))
	for _, identity := range storedRequirement.Authorities {
		if _, affected := searchFinalizationOwner(ancestry.owners, identity.SessionID); !affected {
			return false, ErrAgentFinalizationChanged
		}
		record, err := finterm.ReadAgentFinalization(ctx, tx, identity)
		if err != nil {
			return false, err
		}
		if record.Cancelled {
			return false, context.Canceled
		}
		if !record.Active() {
			return false, ErrAgentFinalizationChanged
		}
		var conflicting bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations
			WHERE session_id=? AND phase IN ('executing', 'waiting_work', 'composing')
			AND (logical_call_id<>? OR generation<>?))`, identity.SessionID, identity.LogicalCallID, identity.Generation).Scan(&conflicting); err != nil {
			return false, err
		}
		if conflicting {
			return false, ErrAgentFinalizationChanged
		}
		byOwner[identity.SessionID] = true
	}
	for _, affected := range ancestry.owners {
		if byOwner[affected] {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, affected).Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			return false, ErrAgentFinalizationChanged
		}
	}
	return true, nil
}

func searchFinalizationOwner(owners []string, owner string) (int, bool) {
	for i, candidate := range owners {
		if candidate == owner {
			return i, true
		}
	}
	return 0, false
}

func verifyQueueCollisionTx(ctx context.Context, tx *sql.Tx, id, owner string, payload finalizationIngressPayload) (bool, error) {
	queueOwner, existingRaw, exists, err := finalizationQueueRowTx(ctx, tx, id)
	if err != nil || !exists {
		return false, err
	}
	if queueOwner != owner {
		return false, ErrAgentFinalizationChanged
	}
	if payload.bound {
		var actual SessionAgentCallData
		if err := json.Unmarshal([]byte(existingRaw), &actual); err != nil {
			return false, err
		}
		actualHash, err := FinalizationPayloadHash(actual)
		if err != nil {
			return false, err
		}
		if actualHash != payload.payloadHash {
			return false, ErrAgentFinalizationChanged
		}
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, owner, id, existingRaw, payload); err != nil {
		return false, err
	}
	return true, nil
}

func persistFinalizationIngressTx(ctx context.Context, tx *sql.Tx, id, owner string, payload finalizationIngressPayload, ancestryHash string) error {
	if !payload.bound {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO session_agent_finalization_continuations
		(row_id, session_id, logical_call_id, payload_hash, identities, ancestry) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(row_id) DO NOTHING`, id, owner, payload.data.LogicalCallID, payload.payloadHash, payload.identities, ancestryHash)
	if err != nil {
		return err
	}
	var storedOwner, logical, hash, identities, ancestry string
	var consumed int64
	if err := tx.QueryRowContext(ctx, `SELECT session_id, logical_call_id, payload_hash, identities, ancestry, consumed
		FROM session_agent_finalization_continuations WHERE row_id=?`, id).Scan(&storedOwner, &logical, &hash, &identities, &ancestry, &consumed); err != nil {
		return err
	}
	if storedOwner != owner || logical != payload.data.LogicalCallID || hash != payload.payloadHash || identities != payload.identities || ancestry != ancestryHash || consumed != 0 {
		return ErrAgentFinalizationChanged
	}
	return nil
}

func finalizationIngressAncestryHash(payload finalizationIngressPayload, admission *BatchFinalizationAdmission) (string, error) {
	if !payload.bound {
		return "", nil
	}
	if admission == nil {
		return "", ErrAgentFinalizationChanged
	}
	for _, identity := range payload.data.FinalizationRequirement.Authorities {
		if !admission.HasOwner(identity.SessionID) {
			return "", fmt.Errorf("%w: authority outside managed ancestry", ErrAgentFinalizationChanged)
		}
	}
	return admission.AncestryHash(), nil
}

// FinalizationFenceError marks an admission or validation failure as a lost
// finalization fence while keeping the cause (including context.Canceled)
// inspectable. Ordinary writer errors must not pass through it.
func FinalizationFenceError(err error) error {
	if err == nil || errors.Is(err, ErrAgentFinalizationChanged) || errors.Is(err, context.Canceled) {
		return err
	}
	return errors.Join(ErrAgentFinalizationChanged, err)
}

// AgentFinalizationContext is an opaque, in-process capability snapshot.
// It is not a serialized identity or permission to bind another incarnation.
type AgentFinalizationContext struct {
	authorities         []*AgentFinalizationAuthority
	managedContinuation bool
}

// RequiresManagedDriver reports lineage verified by the queue writer snapshot.
func (snapshot AgentFinalizationContext) RequiresManagedDriver() bool {
	return snapshot.managedContinuation
}

// FinalizationScoped reports whether the caller carries finalization
// authorities, i.e. whether admission can mint revisions for this call.
func FinalizationScoped(ctx context.Context) bool {
	return len(CaptureAgentFinalizationContext(ctx).authorities) != 0
}

func CaptureAgentFinalizationContext(ctx context.Context) AgentFinalizationContext {
	if ctx == nil {
		return AgentFinalizationContext{}
	}
	authorities, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	managed, _ := ctx.Value(agentFinalizationManagedContinuationKey{}).(bool)
	return AgentFinalizationContext{authorities: slices.Clone(authorities), managedContinuation: managed}
}

// Restore merges without replacing any conflicting authority already present.
func (snapshot AgentFinalizationContext) Restore(ctx context.Context) (context.Context, error) {
	restored, err := WithAgentFinalizationContext(ctx, snapshot.authorities...)
	if err != nil {
		return nil, err
	}
	if snapshot.managedContinuation {
		restored = context.WithValue(restored, agentFinalizationManagedContinuationKey{}, true)
	}
	return restored, nil
}

// RestoreCall refuses a carried owner capability for another logical call.
func (snapshot AgentFinalizationContext) RestoreCall(ctx context.Context, sessionID, logicalID string) (context.Context, error) {
	restored, err := snapshot.Restore(ctx)
	if err != nil {
		return nil, err
	}
	authorities, _ := restored.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	for _, authority := range authorities {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if authority.Identity.SessionID == sessionID && authority.Identity.LogicalCallID != logicalID {
			return nil, ErrAgentFinalizationChanged
		}
		if err := authority.Revalidate(ctx); err != nil {
			return nil, err
		}
	}
	return restored, nil
}

type (
	agentFinalizationAuthoritiesKey         struct{}
	agentFinalizationManagedContinuationKey struct{}
)

// WithAgentFinalizationContext carries bound capabilities, never raw model IDs.
// Descendant calls keep ancestor capabilities until their physical work closes.
func WithAgentFinalizationContext(ctx context.Context, authorities ...*AgentFinalizationAuthority) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("finalization authority context unavailable")
	}
	inherited, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	combined, err := mergeFinalizationAuthorities(inherited, authorities)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, agentFinalizationAuthoritiesKey{}, combined), nil
}

func mergeFinalizationAuthorities(first, second []*AgentFinalizationAuthority) ([]*AgentFinalizationAuthority, error) {
	combined := make([]*AgentFinalizationAuthority, 0, len(first)+len(second))
	byOwner := make(map[string]*AgentFinalizationAuthority)
	for _, set := range [][]*AgentFinalizationAuthority{first, second} {
		for _, authority := range set {
			if authority == nil || authority.Service == nil || !authority.Identity.Valid() {
				return nil, ErrAgentFinalizationChanged
			}
			owner := authority.Identity.SessionID
			if previous := byOwner[owner]; previous != nil {
				if previous.Service != authority.Service || previous.Identity != authority.Identity {
					return nil, ErrAgentFinalizationChanged
				}
				continue
			}
			byOwner[owner] = authority
			combined = append(combined, authority)
		}
	}
	return combined, nil
}

func BatchFinalizationIdentityFromContext(ctx context.Context, id BatchCommitIdentity) (BatchCommitIdentity, error) {
	if ctx == nil {
		return BatchCommitIdentity{}, errors.New("batch finalization authority service unavailable")
	}
	authorities, _ := ctx.Value(agentFinalizationAuthoritiesKey{}).([]*AgentFinalizationAuthority)
	if len(authorities) == 0 {
		return id, nil
	}
	if id.FinalizationService() == nil {
		return BatchCommitIdentity{}, errors.New("batch finalization authority service unavailable")
	}
	_, boundInner := id.FinalizationBinding()
	bound := make([]*AgentFinalizationAuthority, 0, len(boundInner))
	for _, authority := range boundInner {
		var service *service
		for _, contextual := range authorities {
			if contextual != nil && contextual.Service != nil && contextual.Service.finalizationIdentity == id.FinalizationService() {
				service = contextual.Service
				break
			}
		}
		if service == nil || authority.ServiceIdentity() != id.FinalizationService() {
			return BatchCommitIdentity{}, errors.New("batch finalization authority belongs to another service")
		}
		bound = append(bound, &AgentFinalizationAuthority{Service: service, DB: authority.DB, Identity: authority.Identity, inner: authority})
	}
	combined, err := mergeFinalizationAuthorities(bound, authorities)
	if err != nil {
		return BatchCommitIdentity{}, err
	}
	inner := make([]*sessguard.AgentFinalizationAuthority, 0, len(combined))
	for _, authority := range combined {
		if authority.Service == nil || authority.Service.finalizationIdentity != id.FinalizationService() {
			return BatchCommitIdentity{}, errors.New("batch finalization authority belongs to another service")
		}
		innerAuthority := authority.Service.sessguardAuthority(authority)
		if innerAuthority == nil {
			return BatchCommitIdentity{}, errors.New("batch finalization authority belongs to another service")
		}
		inner = append(inner, innerAuthority)
	}
	return serviceIdentity(id, sessguard.AttachFinalizationAuthorities(componentIdentity(id), id.FinalizationService(), inner...)), nil
}

// ErrFinalizationBatchResultReadMissing is returned when a gated batch-result
// wrapper is built without an underlying read.
var ErrFinalizationBatchResultReadMissing = errors.New(
	"finalization batch result read is missing")

// FinalizationTerminalFinishReason reports whether a finish reason ends the
// agent's turn instead of continuing it. It mirrors isTerminalFinishReason in
// internal/cmd/sessions_watch.go: end_turn, max_tokens, canceled and error are
// terminal; tool_use and unknown mean the loop is still mid-step.
func FinalizationTerminalFinishReason(reason message.FinishReason) bool {
	switch reason {
	case message.FinishReasonEndTurn,
		message.FinishReasonMaxTokens,
		message.FinishReasonCanceled,
		message.FinishReasonError:
		return true
	default:
		return false
	}
}

// FinalizationTerminalMessage reports whether a message event is the terminal
// answer of its session: an assistant message carrying a non-partial Finish
// whose reason is terminal. Partial checkpoint finishes (the provisional
// finalization candidate) are never terminal.
func FinalizationTerminalMessage(m message.Message) bool {
	if m.Role != message.Assistant {
		return false
	}
	finish := m.FinishPart()
	if finish == nil || finish.Partial {
		return false
	}
	return FinalizationTerminalFinishReason(finish.Reason)
}

// FinalizationSessionEnded reports whether a session event payload marks the
// end of the session: a non-empty EndedReason on the row.
func FinalizationSessionEnded(s Session) bool {
	return s.EndedReason != ""
}

// FinalizationBatchResultRead reads the owner's batch completion; it matches
// (*BatchStore).BatchDeliveryOneItemCompletion and
// (*BatchStore).BatchDeliveryRootEnvelope so either binds as a method value.
// ok=false means no deliverable completion yet, error is a read failure.
type FinalizationBatchResultRead func(
	ctx context.Context, owner, root string,
) (sesstypes.BatchDeliveryEnvelope, bool, error)

// FinalizationGatedBatchResult filters ONE batch-result read through the
// owner's terminal gate: while the owner holds an active finalization record
// the completion is withheld as (sesstypes.BatchDeliveryEnvelope{}, false, nil), which
// the batch consumers' durable poll (LoadDone/AwaitDone) already treats as
// "keep waiting", so no consumer can observe a batch result before the
// parent's answer is sealed. A nil gate is the inert wiring: the read result
// is returned byte-for-byte unchanged. A read failure and a canceled context
// are returned as errors, never as a delivered result.
func FinalizationGatedBatchResult(
	ctx context.Context,
	gate FinalizationTerminalGate,
	owner, root string,
	read FinalizationBatchResultRead,
) (sesstypes.BatchDeliveryEnvelope, bool, error) {
	if read == nil {
		return sesstypes.BatchDeliveryEnvelope{}, false, ErrFinalizationBatchResultReadMissing
	}
	// Inert passthrough: a nil gate must not touch the context or the gate.
	if gate == nil {
		return read(ctx, owner, root)
	}
	if err := ctx.Err(); err != nil {
		return sesstypes.BatchDeliveryEnvelope{}, false, err
	}
	// Fail-closed withholding: an unreadable gate store also refuses here.
	if !gate.AllowTerminal(ctx, owner) {
		return sesstypes.BatchDeliveryEnvelope{}, false, nil
	}
	return read(ctx, owner, root)
}

// FinalizationAwaitGatedBatchResult blocks until the owner's record leaves the
// active phases (AwaitSealed) and then reads the completion once. A nil gate
// is the inert default and never waits. AwaitSealed errors — cancellation or
// a store read failure — are returned as errors and the read never runs, so a
// broken gate can never turn into a delivered "success".
func FinalizationAwaitGatedBatchResult(
	ctx context.Context,
	gate FinalizationTerminalGate,
	owner, root string,
	read FinalizationBatchResultRead,
) (sesstypes.BatchDeliveryEnvelope, bool, error) {
	if read == nil {
		return sesstypes.BatchDeliveryEnvelope{}, false, ErrFinalizationBatchResultReadMissing
	}
	if gate == nil {
		gate = DefaultFinalizationTerminalGate()
	}
	if err := gate.AwaitSealed(ctx, owner); err != nil {
		return sesstypes.BatchDeliveryEnvelope{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return sesstypes.BatchDeliveryEnvelope{}, false, err
	}
	return read(ctx, owner, root)
}

// Standalone read paths served through the read-only connection: Get,
// GetLast, List, ListAll, sub-session listing, and call-tree activity
// queries. These publish no pubsub events and need no
// read-your-own-write consistency with a later write.

func (s *service) Get(ctx context.Context, id string) (Session, error) {
	dbSession, err := s.qRead.GetSessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
}

func (s *service) GetLast(ctx context.Context) (Session, error) {
	// WS-1 (#1142 step C): only sessions this process owns are candidates for
	// `--continue`, so a shared data directory never hands a linked worktree
	// (or the main checkout) another workspace's recent history.
	workspaceRoot, home := s.ownsArgs()
	dbSession, err := s.qRead.GetLastSession(ctx, workspaceRoot, home)
	if err != nil {
		return Session{}, err
	}
	return s.fromDBItem(dbSession), nil
}

// ListSubSessions implementation: thin wrapper around the sqlc-
// generated query. Returns an empty slice when no sub-sessions exist.
func (s *service) ListSubSessions(ctx context.Context, parentSessionID string) ([]Session, error) {
	dbSessions, err := s.qRead.ListSubSessions(ctx, sql.NullString{
		String: parentSessionID,
		Valid:  parentSessionID != "",
	})
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
	}
	return sessions, nil
}

// GetCallTreeActivity implementation: see the Service interface doc. A
// sql.ErrNoRows result (no messages anywhere in the tree) is reported as
// (zero-value, false, nil) rather than propagated as an error — an empty
// tree is a normal, expected state (e.g. a session that was just created),
// not a failure.
func (s *service) GetCallTreeActivity(ctx context.Context, rootID string) (CallTreeActivity, bool, error) {
	row, err := s.qRead.GetCallTreeActivity(ctx, rootID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CallTreeActivity{}, false, nil
		}
		return CallTreeActivity{}, false, err
	}
	return CallTreeActivity{
		SessionID:  row.SessionID,
		Role:       row.Role,
		LatestUnix: row.LatestUnix,
	}, true, nil
}

// callTreeActivityBatchChunkSize caps how many root IDs are passed to the
// underlying sqlc-generated GetCallTreeActivityBatch in a single query. The
// generated query expands rootIDs via sqlc.slice into one SQL parameter per
// id, so an unbounded list would eventually hit SQLite's
// SQLITE_MAX_VARIABLE_NUMBER ceiling (999 on older builds). 500 stays well
// below that with headroom for the query's other bound parameters, and keeps
// each recursive-CTE fan-out bounded to a reasonable batch. Because every
// root's tree is independent (the CTE partitions by root_session_id),
// splitting roots across chunks and merging the per-root maps is exact.
const callTreeActivityBatchChunkSize = 500

// GetCallTreeActivityBatch implementation: see the Service interface doc.
// rootIDs are split into callTreeActivityBatchChunkSize-sized chunks, each run
// as a separate underlying query, and the per-root results merged into one map.
func (s *service) GetCallTreeActivityBatch(ctx context.Context, rootIDs []string) (map[string]CallTreeActivity, error) {
	out := make(map[string]CallTreeActivity, len(rootIDs))
	if len(rootIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(rootIDs); start += callTreeActivityBatchChunkSize {
		end := start + callTreeActivityBatchChunkSize
		if end > len(rootIDs) {
			end = len(rootIDs)
		}
		rows, err := s.qRead.GetCallTreeActivityBatch(ctx, rootIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[row.RootSessionID] = CallTreeActivity{
				SessionID:  row.SessionID,
				Role:       row.Role,
				LatestUnix: row.LatestUnix,
			}
		}
	}
	return out, nil
}

func (s *service) List(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.qRead.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
	}
	return sessions, nil
}

// Fork merge note (origin/main 2736e487 "fix(ui): mark estimated context
// usage" + 9595d1f0 "fix(session): preserve estimated usage marker"):
// upstream added applyEstimatedUsageState / setEstimatedUsageState /
// clearEstimatedUsageState as backend infrastructure for their TUI's
// "estimated context usage" marker. Rejected — the whole feature drives
// a TUI widget we do not ship; our WebUI handles usage display via the
// WebSocket events stream (internal/server/events.go) without per-session
// estimated-state tracking. See CHANGELOG.fork.md Section 2.
func (s *service) ListAll(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.qRead.ListAllSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
	}
	return sessions, nil
}

// Workspace ownership (WS-1, task #1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md sec.1): a session belongs to the
// workspace of the process that created it, and only that process may drive
// it. The predicate below is the single Go-side definition of "owns"; the SQL
// ownership filters (GetLastSession, ListPendingRunQueueEntries,
// ListDueWakeSchedules, NextDueWakeScheduleAt) carry the same predicate as
// their (workspace_root, home) parameters -- keep the two in sync.

// Owns reports whether a process may drive session S. A session is owned when
// its workspace_root equals the process's workspace, or when it is a legacy
// unbound row (”) and the process is a home process -- the owner of its own
// data directory. A checkout inside git has a non-empty workspace root and is
// still a home process: home is NOT derivable from the root (it is false only
// for a linked worktree on the shared data directory, SD-D #1143), and every
// production caller passes config.WorkspaceHome().
//
// procHome is therefore an explicit flag from the app layer, never a
// re-derivation: a home process (every process but a shared-from-linked one)
// must keep driving every pre-existing legacy row, or "--continue", the pump and the wake
// scheduler would all refuse the history they already own.
func Owns(sessWorkspaceRoot, procWorkspaceRoot string, procHome bool) bool {
	if sessWorkspaceRoot == procWorkspaceRoot {
		// Same workspace (a linked worktree may also see rows sharing its
		// root): the normal case, plus the legacy case of two empty roots.
		return true
	}
	if sessWorkspaceRoot != "" {
		// Another workspace's row: never ours, whatever we are.
		return false
	}
	// A legacy '' row: only a home process may run it; a shared-from-linked
	// process (home=false) refuses instead.
	return procHome
}

// gitBranchFromWorkDir reads the branch HEAD points at, resolved against the
// git directory of dir -- WITHOUT spawning git (the design doc forbids a
// subprocess on the session-creation path). A .git DIRECTORY contributes its
// HEAD; a .git FILE ("gitdir: X") the HEAD of X; anything else (no git, an
// unreadable HEAD, a detached HEAD) yields "". Errors are never fatal: the
// branch is audit attribution, not ownership.
func gitBranchFromWorkDir(dir string) string {
	if dir == "" {
		return ""
	}
	gitDir, ok := locateGitDir(dir)
	if !ok {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(head))
	const refPrefix = "ref: "
	if !strings.HasPrefix(line, refPrefix) {
		return "" // detached HEAD
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, refPrefix), "refs/heads/")
}

// locateGitDir walks up from dir looking for the nearest .git entry and
// returns the resolved git directory, following the "gitdir: <path>"
// indirection of linked worktrees and --separate-git-dir.
func locateGitDir(dir string) (string, bool) {
	path := filepath.Clean(dir)
	for {
		entry := filepath.Join(path, ".git")
		fi, err := os.Stat(entry)
		switch {
		case err == nil && fi.IsDir():
			return entry, true
		case err == nil:
			raw, rerr := os.ReadFile(entry)
			if rerr != nil {
				return "", false
			}
			line := strings.TrimSpace(string(raw))
			const prefix = "gitdir: "
			if !strings.HasPrefix(line, prefix) {
				return "", false
			}
			resolved := strings.TrimPrefix(line, prefix)
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(path, resolved)
			}
			return filepath.Clean(resolved), true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", false
		}
		path = parent
	}
}

// ownsArgs are the (workspace_root, home) parameters every ownership-filtered
// query takes, computed once per call from the service's wiring. The home
// flag is the explicit constructor value (config.WorkspaceHome() in
// production) -- NEVER a re-derivation from the workspace root, which would
// refuse every pre-existing legacy row to every git-checkout process.
func (s *service) ownsArgs() (workspaceRoot string, home int64) {
	return s.workspaceRoot, HomeFlag(s.home)
}

func HomeFlag(home bool) int64 {
	if home {
		return 1
	}
	return 0
}
