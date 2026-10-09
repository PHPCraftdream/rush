package tasktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

type Service struct {
	store  Store
	limits Limits
}

func NewService(store Store, limits Limits) *Service {
	return &Service{store: store, limits: limits}
}

func serviceProblem(code ProblemCode, message string) error {
	return &Problem{Code: code, Message: message}
}

func serviceIdentity(value string) bool {
	return strings.TrimSpace(value) != ""
}

func serviceActor(actor Actor) bool {
	return (actor.Kind == ActorAgent || actor.Kind == ActorOperator) && serviceIdentity(actor.ID)
}

func CloneEnvelope(envelope Envelope) Envelope {
	envelope.Snapshot = CloneSnapshot(envelope.Snapshot)
	if envelope.Receipts != nil {
		receipts := make(map[RequestID]Receipt, len(envelope.Receipts))
		for id, receipt := range envelope.Receipts {
			receipts[id] = serviceCloneReceipt(receipt)
		}
		envelope.Receipts = receipts
	}
	return envelope
}

func serviceCloneReceipt(receipt Receipt) Receipt {
	receipt.Delta.Created = append([]NodeID(nil), receipt.Delta.Created...)
	receipt.Delta.Updated = append([]NodeID(nil), receipt.Delta.Updated...)
	receipt.Delta.Removed = append([]NodeID(nil), receipt.Delta.Removed...)
	receipt.Delta.Completed = append([]NodeID(nil), receipt.Delta.Completed...)
	return receipt
}

func CheckEnvelope(envelope Envelope, limits Limits) error {
	if err := ValidateSnapshot(envelope.Snapshot, limits); err != nil {
		return err
	}
	invalid := func(message string) error { return serviceProblem(CodeInvalidSnapshot, message) }
	if len(envelope.Receipts) > limits.MaxReceipts {
		return invalid("receipt history exceeds limit")
	}
	if envelope.Revision == 0 {
		s := envelope.Snapshot
		if len(envelope.Receipts) != 0 || s.Initialized || s.NextID != 1 || len(s.Nodes) != 1 || len(s.Tombstones) != 0 || len(s.TitleGuards) != 0 {
			return invalid("revision zero must describe an absent board")
		}
		return nil
	}
	ordered := make([]Receipt, 0, len(envelope.Receipts))
	seenRevision := make(map[Revision]bool, len(ordered))
	for id, receipt := range envelope.Receipts {
		if !serviceIdentity(string(id)) || receipt.RequestID != id || !serviceActor(receipt.Actor) {
			return invalid("invalid receipt identity")
		}
		fingerprint, err := hex.DecodeString(receipt.Fingerprint)
		if err != nil || len(fingerprint) != sha256.Size || hex.EncodeToString(fingerprint) != receipt.Fingerprint {
			return invalid("receipt fingerprint is not canonical lowercase SHA-256")
		}
		r := receipt.CommittedRevision
		if r == 0 || r > envelope.Revision || seenRevision[r] {
			return invalid("invalid or duplicate receipt revision")
		}
		seenRevision[r] = true
		ordered = append(ordered, receipt)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].CommittedRevision < ordered[j].CommittedRevision })
	created := make(map[NodeID]bool)
	removed := make(map[NodeID]bool)
	for _, receipt := range ordered {
		sets := make(map[NodeID]int)
		for category, ids := range [][]NodeID{receipt.Delta.Created, receipt.Delta.Updated, receipt.Delta.Removed} {
			for _, id := range ids {
				_, live := envelope.Snapshot.Nodes[id]
				_, dead := envelope.Snapshot.Tombstones[id]
				if (!live && !dead) || sets[id] != 0 || removed[id] {
					return invalid("receipt delta has unknown, duplicate, overlapping, or already removed ID")
				}
				sets[id] = category + 1
				if category == 0 {
					if id == RootID || created[id] {
						return invalid("receipt recreates an ID")
					}
					created[id] = true
				}
				if category == 2 {
					if id == RootID || !dead {
						return invalid("removed receipt ID has no tombstone")
					}
					if receipt.Actor.Kind != ActorOperator || receipt.Actor != envelope.Snapshot.Tombstones[id].Actor {
						return invalid("removed receipt actor must match operator tombstone actor")
					}
					removed[id] = true
				}
			}
		}
		completed := make(map[NodeID]bool)
		for _, id := range receipt.Delta.Completed {
			node, live := envelope.Snapshot.Nodes[id]
			tombstone := envelope.Snapshot.Tombstones[id]
			if completed[id] || sets[id] != 2 || (live && node.Kind != KindTask) || (!live && tombstone.Kind != KindTask) {
				return invalid("completed IDs must be distinct updated tasks")
			}
			completed[id] = true
		}
	}
	return nil
}

func (s *Service) load(ctx context.Context, key TreeKey) (Envelope, *Tree, error) {
	if !serviceIdentity(string(key)) {
		return Envelope{}, nil, serviceProblem(CodeInvalidInput, "tree key is required")
	}
	if err := ctx.Err(); err != nil {
		return Envelope{}, nil, err
	}
	if err := ValidateLimits(s.limits); err != nil {
		return Envelope{}, nil, err
	}
	if s.store == nil {
		return Envelope{}, nil, &InfrastructureError{Operation: "load", Err: errors.New("nil store")}
	}
	envelope, err := s.store.Load(ctx, key)
	if err != nil {
		return Envelope{}, nil, &InfrastructureError{Operation: "load", Err: err}
	}
	if err := CheckEnvelope(envelope, s.limits); err != nil {
		return Envelope{}, nil, &InfrastructureError{Operation: "validate loaded envelope", Err: err}
	}
	return envelope, kernelOwnedTree(envelope.Snapshot, s.limits), nil
}

func serviceSummary(tree *Tree, key TreeKey, revision Revision) Summary {
	summary := tree.Summary()
	summary.TreeKey, summary.Revision = key, revision
	return summary
}

func (s *Service) Summary(ctx context.Context, key TreeKey) (Summary, error) {
	envelope, tree, err := s.load(ctx, key)
	if err != nil {
		return Summary{}, err
	}
	return serviceSummary(tree, key, envelope.Revision), nil
}

func (s *Service) View(ctx context.Context, key TreeKey, selector Selector) (View, error) {
	envelope, tree, err := s.load(ctx, key)
	if err != nil {
		return View{}, err
	}
	view, err := tree.View(selector)
	if err != nil {
		return View{}, err
	}
	view.Summary.TreeKey, view.Summary.Revision = key, envelope.Revision
	return view, nil
}

func serviceFingerprint(expected Revision, command Command) (string, error) {
	payload, err := json.Marshal(struct {
		Expected Revision `json:"expected_revision"`
		Command  Command  `json:"command"`
	}{expected, command})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func serviceReplay(envelope Envelope, tree *Tree, key TreeKey, actor Actor, request RequestID, fingerprint string) (MutationReply, bool, error) {
	receipt, ok := envelope.Receipts[request]
	if !ok {
		return MutationReply{}, false, nil
	}
	if receipt.Actor != actor || receipt.Fingerprint != fingerprint {
		return MutationReply{}, true, serviceProblem(CodeRequestReused, "request ID belongs to another actor or payload; use a new request ID")
	}
	return MutationReply{
		Receipt: serviceCloneReceipt(receipt), Replayed: true,
		Summary: serviceSummary(tree, key, envelope.Revision),
		Created: tree.Briefs(receipt.Delta.Created),
	}, true, nil
}

func serviceConflict(expected, current Revision) error {
	return &Problem{Code: CodeConflict, Message: "board changed; reread and submit newly considered intent with a new request ID", Expected: expected, Current: current}
}

func (s *Service) Mutate(ctx context.Context, key TreeKey, actor Actor, request RequestID, expected Revision, command Command) (MutationReply, error) {
	if !serviceIdentity(string(key)) || !serviceIdentity(string(request)) || !serviceActor(actor) {
		return MutationReply{}, serviceProblem(CodeInvalidInput, "trusted tree key, actor and request ID are required")
	}
	if command.Op == OpView {
		return MutationReply{}, serviceProblem(CodeInvalidInput, "use View for read-only requests")
	}
	fingerprint, err := serviceFingerprint(expected, command)
	if err != nil {
		return MutationReply{}, serviceProblem(CodeInvalidInput, err.Error())
	}
	envelope, tree, err := s.load(ctx, key)
	if err != nil {
		return MutationReply{}, err
	}
	if reply, found, err := serviceReplay(envelope, tree, key, actor, request, fingerprint); found {
		return reply, err
	}
	if actor.Kind != ActorOperator && (command.Op == OpRemove || command.Op == OpReopen) {
		return MutationReply{}, serviceProblem(CodeForbidden, "only operators may remove or reopen")
	}
	if expected != envelope.Revision {
		return MutationReply{}, serviceConflict(expected, envelope.Revision)
	}
	if expected == ^Revision(0) || len(envelope.Receipts) >= s.limits.MaxReceipts {
		return MutationReply{}, serviceProblem(CodeLimitExceeded, "revision or receipt history exhausted")
	}
	delta, err := tree.Apply(actor, command)
	if err != nil {
		return MutationReply{}, err
	}
	receipt := Receipt{RequestID: request, Actor: actor, Fingerprint: fingerprint, CommittedRevision: expected + 1, Delta: delta}
	reply := MutationReply{Receipt: serviceCloneReceipt(receipt), Summary: serviceSummary(tree, key, expected+1), Created: tree.Briefs(delta.Created)}
	candidate := Envelope{Revision: expected + 1, Snapshot: tree.takeSnapshot(), Receipts: envelope.Receipts}
	if candidate.Receipts == nil {
		candidate.Receipts = make(map[RequestID]Receipt)
	}
	candidate.Receipts[request] = receipt
	if err := CheckEnvelope(candidate, s.limits); err != nil {
		return MutationReply{}, &InfrastructureError{Operation: "validate candidate", Err: err}
	}
	if err := ctx.Err(); err != nil {
		return MutationReply{}, err
	}
	if err := s.store.Commit(ctx, key, expected, candidate); err != nil {
		var unknown *CommitUnknownError
		var infrastructure *InfrastructureError
		var problem *Problem
		if errors.As(err, &unknown) || errors.As(err, &infrastructure) {
			return MutationReply{}, err
		}
		if errors.As(err, &problem) && problem.Code == CodeConflict {
			current, currentTree, loadErr := s.load(ctx, key)
			if loadErr != nil {
				return MutationReply{}, loadErr
			}
			if replay, found, replayErr := serviceReplay(current, currentTree, key, actor, request, fingerprint); found {
				return replay, replayErr
			}
			return MutationReply{}, serviceConflict(expected, current.Revision)
		}
		return MutationReply{}, &InfrastructureError{Operation: "commit", Err: err}
	}
	return reply, nil
}
