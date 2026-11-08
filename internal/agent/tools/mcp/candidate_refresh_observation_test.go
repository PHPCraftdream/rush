package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

type candidateRefreshObservation struct {
	rawEvents        int
	refreshPublished bool
	events           []Event
}

func (o *candidateRefreshObservation) record(event Event, name string) error {
	if len(o.events) == 16 {
		o.events = o.events[1:]
	}
	o.events = append(o.events, event)
	if event.Name != name {
		return nil
	}
	if event.Type == EventToolsListChanged {
		o.rawEvents++
		if o.rawEvents > 1 {
			return fmt.Errorf("candidate notification duplicated: %d", o.rawEvents)
		}
	}
	if event.Type == EventStateChanged && event.State == StateConnected && event.Counts.Tools == 1 {
		o.refreshPublished = true
	}
	return nil
}

func collectCandidateRefreshEvents(ctx context.Context, events <-chan pubsub.Event[Event], name string) (candidateRefreshObservation, error) {
	var observation candidateRefreshObservation
	// Refresh-worker может обогнать публикацию исходного уведомления.
	for !observation.refreshPublished || observation.rawEvents == 0 {
		select {
		case event, ok := <-events:
			if !ok {
				return observation, fmt.Errorf("candidate event stream closed before both facts")
			}
			if err := observation.record(event.Payload, name); err != nil {
				return observation, err
			}
		case <-ctx.Done():
			return observation, ctx.Err()
		}
	}
	// Commit уже вернулся: все его raw events опубликованы, дубликаты нельзя пропускать.
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return observation, nil
			}
			if err := observation.record(event.Payload, name); err != nil {
				return observation, err
			}
		default:
			return observation, nil
		}
	}
}

func candidateObservationEvents(events ...Event) <-chan pubsub.Event[Event] {
	ch := make(chan pubsub.Event[Event], len(events))
	for _, event := range events {
		ch <- pubsub.Event[Event]{Payload: event}
	}
	close(ch)
	return ch
}

func TestCandidateRefreshObservationAcceptsBothOrders(t *testing.T) {
	const name = "candidate"
	raw := Event{Type: EventToolsListChanged, Name: name}
	refreshed := Event{Type: EventStateChanged, Name: name, State: StateConnected, Counts: Counts{Tools: 1}}
	for _, tc := range []struct {
		name   string
		events []Event
	}{
		{name: "notification-first", events: []Event{raw, refreshed}},
		{name: "refresh-first", events: []Event{refreshed, raw}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation, err := collectCandidateRefreshEvents(t.Context(), candidateObservationEvents(tc.events...), name)
			require.NoError(t, err)
			require.Equal(t, 1, observation.rawEvents)
			require.True(t, observation.refreshPublished)
		})
	}
}

func TestCandidateRefreshObservationRejectsMissingOrDuplicateFacts(t *testing.T) {
	const name = "candidate"
	raw := Event{Type: EventToolsListChanged, Name: name}
	refreshed := Event{Type: EventStateChanged, Name: name, State: StateConnected, Counts: Counts{Tools: 1}}
	for _, tc := range []struct {
		name   string
		events []Event
	}{
		{name: "no-notification", events: []Event{refreshed}},
		{name: "no-refresh", events: []Event{raw}},
		{name: "wrong-owner", events: []Event{refreshed, {Type: EventToolsListChanged, Name: "other"}}},
		{name: "duplicate-before-refresh", events: []Event{raw, raw, refreshed}},
		{name: "duplicate-after-both", events: []Event{refreshed, raw, raw}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectCandidateRefreshEvents(t.Context(), candidateObservationEvents(tc.events...), name)
			require.Error(t, err)
		})
	}
}

func TestCandidateRefreshObservationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := collectCandidateRefreshEvents(ctx, make(chan pubsub.Event[Event]), "candidate")
	require.ErrorIs(t, err, context.Canceled)
}
