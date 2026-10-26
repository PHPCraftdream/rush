package lab

import (
	"reflect"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Validate shape even when missing history prevents an Engine reconstruction.
func journalRecordMetadataValid(r jobbatch.Record) bool {
	if !scenarioRecordValid(r.Type) || !jobbatch.ValidCode(r.Code) {
		return false
	}
	want := jobbatch.Record{Cursor: r.Cursor, Sequence: r.Sequence, Type: r.Type}
	switch r.Type {
	case jobbatch.RecordSubmit:
		want.Root, want.Node, want.State, want.Cause, want.Ref, want.IDs = r.Root, r.Node, r.State, r.Cause, r.Ref, r.IDs
		if r.Root == "" || r.Node != r.Root || !scenarioStateValid(r.State) || r.Ref == "" || len(r.IDs) == 0 {
			return false
		}
	case jobbatch.RecordAdd:
		want.Root, want.Node, want.State, want.Cause, want.Ref, want.IDs = r.Root, r.Node, r.State, r.Cause, r.Ref, r.IDs
		if r.Root == "" || r.Node == "" || r.Ref == "" {
			return false
		}
	case jobbatch.RecordRelease:
		want.Root, want.Node = r.Root, r.Node
		if r.Root == "" || r.Node != r.Root {
			return false
		}
	case jobbatch.RecordCapacityAvailable:
		want.Group = r.Group
	case jobbatch.RecordHostClosing, jobbatch.RecordClosed:
	case jobbatch.RecordCommand:
		want.Command, want.Node, want.Token, want.Cause = r.Command, r.Node, r.Token, r.Cause
		if (r.Command != jobbatch.CommandStartLeaf && r.Command != jobbatch.CommandStopLeaf) || r.Node == "" || r.Token == 0 {
			return false
		}
		if r.Command == jobbatch.CommandStartLeaf && r.Cause != "" {
			return false
		}
	case jobbatch.RecordNotice:
		want.Root, want.Node, want.Kind, want.State, want.Cause, want.Counts = r.Root, r.Node, r.Kind, r.State, r.Cause, r.Counts
		if r.Root == "" || r.Node == "" || (r.Kind != jobbatch.KindTask && r.Kind != jobbatch.KindBatch && r.Kind != jobbatch.KindRoot) || !r.State.Terminal() || r.Counts == nil || !scenarioCountsValid(r.Counts) {
			return false
		}
	default:
		want.Root, want.Node, want.Token, want.State, want.Cause, want.Group = r.Root, r.Node, r.Token, r.State, r.Cause, r.Group
		if !scenarioStateValid(r.State) {
			return false
		}
		if r.Root == "" || r.Node == "" {
			return false
		}
		switch r.Type {
		case jobbatch.RecordStopRequested:
		case jobbatch.RecordStarted, jobbatch.RecordDeadlineExpired:
			if r.Token == 0 {
				return false
			}
		case jobbatch.RecordSettled:
			want.State, want.Code = r.State, r.Code
			if r.Token == 0 || !r.State.Terminal() {
				return false
			}
		case jobbatch.RecordStartRejected:
			want.Reason, want.Code = r.Reason, r.Code
			if r.Token == 0 || (r.Reason != jobbatch.RejectCapacity && r.Reason != jobbatch.RejectFailure && r.Reason != jobbatch.RejectCancelled) {
				return false
			}
		case jobbatch.RecordControl:
			want.Op, want.Code = r.Op, r.Code
			if r.Token == 0 || r.Op != jobbatch.ControlOpStop {
				return false
			}
		}
	}
	if !scenarioCauseValid(&r.Cause) {
		return false
	}
	ids := map[jobbatch.ID]bool{}
	for _, id := range r.IDs {
		if id == "" || ids[id] {
			return false
		}
		ids[id] = true
	}
	return reflect.DeepEqual(want, r)
}
