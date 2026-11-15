package jobbatch

import (
	"fmt"
	"reflect"
	"testing"
)

// The feed is a ring: an overflowing append overwrites the oldest slot and
// advances start instead of shifting the whole slice.
func TestRunnerObservationFeedRingRotatesAfterOverflow(t *testing.T) {
	r := &Runner{feed: runnerFeed{retention: 3, changed: make(chan struct{})}}
	for i := 0; i < 5; i++ {
		r.appendRecords([]Record{{Sequence: uint64(i + 1), Type: RecordCapacityAvailable}}, false)
	}
	if r.feed.start != 2 {
		t.Fatalf("ring start after overflow = %d, want 2", r.feed.start)
	}
	page, err := r.Events(t.Context(), 3)
	if err != nil || len(page.Records) != 3 || page.Oldest != 3 || page.Records[0].Cursor != 3 || page.Records[2].Cursor != 5 {
		t.Fatalf("retained page = %+v, err %v", page, err)
	}
}

// Differential against the shifting-slice model the ring replaced: after every
// append, every cursor reads the same page (records, Next, Head) or the same gap.
func TestRunnerObservationFeedMatchesShiftSemantics(t *testing.T) {
	for _, retention := range []int{1, 3} {
		t.Run(fmt.Sprintf("retention_%d", retention), func(t *testing.T) {
			r := &Runner{feed: runnerFeed{retention: retention, changed: make(chan struct{})}}
			var want []Record
			for i := 0; i < 8; i++ {
				record := Record{Sequence: uint64(i + 1), Type: RecordCapacityAvailable}
				r.appendRecords([]Record{record}, false)
				record.Cursor = uint64(i + 1)
				if len(want) == retention {
					want = want[1:]
				}
				want = append(want, record)
				for cursor := uint64(1); cursor <= uint64(i+1); cursor++ {
					page, err := r.Events(t.Context(), cursor)
					if cursor < want[0].Cursor {
						if err != ErrGap || page.Oldest != want[0].Cursor || page.Next != want[0].Cursor {
							t.Fatalf("retention=%d cursor=%d gap page=%+v err=%v", retention, cursor, page, err)
						}
						continue
					}
					wantRecords := want[int(cursor-want[0].Cursor):]
					if err != nil || !reflect.DeepEqual(page.Records, wantRecords) ||
						page.Next != want[len(want)-1].Cursor+1 || page.Head != uint64(i+1) {
						t.Fatalf("retention=%d cursor=%d page=%+v err=%v, want records %+v", retention, cursor, page, err, wantRecords)
					}
				}
			}
		})
	}
}

func TestRunnerObservationFeedGap(t *testing.T) {
	r := &Runner{feed: runnerFeed{retention: 2, changed: make(chan struct{})}}
	for i := 0; i < 4; i++ {
		r.appendRecords([]Record{{Type: RecordCapacityAvailable}}, false)
	}
	page, err := r.Events(t.Context(), 1)
	if err != ErrGap || page.Oldest != 3 || page.Next != 3 || len(page.Records) != 0 {
		t.Fatalf("gap page = %+v, err %v", page, err)
	}
}
