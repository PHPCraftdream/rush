package server

import (
	"encoding/json"
	"sync"
	"time"
)

const (
	apiEventMaxPerSession = 1000
	apiEventMaxSessions   = 200
	apiEventSubscriberCap = 256
)

type APIEvent struct {
	ID   uint64          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	At   time.Time       `json:"at"`
}

type apiEventSession struct {
	events           []APIEvent
	subscribers      map[*apiEventSubscriber]struct{}
	lastUse          uint64
	truncatedThrough uint64
}

type apiEventSubscriber struct {
	ch       chan APIEvent
	overflow chan struct{}
	dropped  bool
}

// APIEventLog retains a bounded, route-set-local replay window per session.
type APIEventLog struct {
	mu            sync.Mutex
	sessions      map[string]*apiEventSession
	now           func() time.Time
	maxPerSession int
	nextID        uint64
	useSeq        uint64
}

func NewAPIEventLog(now func() time.Time) *APIEventLog {
	if now == nil {
		now = time.Now
	}
	return &APIEventLog{sessions: make(map[string]*apiEventSession), now: now, maxPerSession: apiEventMaxPerSession}
}

func (l *APIEventLog) Publish(sessionID, eventType string, data any) (APIEvent, bool) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return APIEvent{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sessionLocked(sessionID)
	if s == nil {
		return APIEvent{}, false
	}
	l.nextID++
	event := APIEvent{ID: l.nextID, Type: eventType, Data: encoded, At: l.now().UTC()}
	s.events = append(s.events, event)
	if len(s.events) > l.maxPerSession {
		s.truncatedThrough = s.events[0].ID
		s.events = s.events[1:]
	}
	for subscriber := range s.subscribers {
		if subscriber.dropped {
			continue
		}
		select {
		case subscriber.ch <- event:
		default:
			subscriber.dropped = true
			delete(s.subscribers, subscriber)
			close(subscriber.overflow)
		}
	}
	return event, true
}

func (l *APIEventLog) Replay(sessionID string, after uint64) (events []APIEvent, oldest uint64, retained bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sessions[sessionID]
	if s == nil {
		return nil, 0, false
	}
	l.touchLocked(s)
	if len(s.events) == 0 {
		return nil, s.truncatedThrough + 1, true
	}
	oldest = s.events[0].ID
	if s.truncatedThrough > 0 {
		oldest = s.truncatedThrough + 1
	}
	for _, event := range s.events {
		if event.ID > after {
			events = append(events, cloneAPIEvent(event))
		}
	}
	return events, oldest, true
}

func (l *APIEventLog) Subscribe(sessionID string) (<-chan APIEvent, <-chan struct{}, func()) {
	l.mu.Lock()
	s := l.sessionLocked(sessionID)
	if s == nil {
		l.mu.Unlock()
		return nil, nil, func() {}
	}
	subscriber := &apiEventSubscriber{ch: make(chan APIEvent, apiEventSubscriberCap), overflow: make(chan struct{})}
	s.subscribers[subscriber] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			l.mu.Lock()
			delete(s.subscribers, subscriber)
			l.mu.Unlock()
		})
	}
	return subscriber.ch, subscriber.overflow, unsubscribe
}

func (l *APIEventLog) DeleteSession(sessionID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.sessions[sessionID]; s != nil {
		for subscriber := range s.subscribers {
			delete(s.subscribers, subscriber)
			close(subscriber.ch)
		}
		delete(l.sessions, sessionID)
	}
}

func (l *APIEventLog) SessionCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sessions)
}

func (l *APIEventLog) SubscriberCount(sessionID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.sessions[sessionID]; s != nil {
		return len(s.subscribers)
	}
	return 0
}

func (l *APIEventLog) sessionLocked(id string) *apiEventSession {
	if s := l.sessions[id]; s != nil {
		l.touchLocked(s)
		return s
	}
	if len(l.sessions) >= apiEventMaxSessions {
		oldestID := ""
		var oldest *apiEventSession
		for key, candidate := range l.sessions {
			if len(candidate.subscribers) != 0 {
				continue
			}
			if oldest == nil || candidate.lastUse < oldest.lastUse {
				oldestID, oldest = key, candidate
			}
		}
		if oldest != nil {
			delete(l.sessions, oldestID)
		}
	}
	// Soft cap: when every retained session has a live stream nothing may be
	// evicted, and dropping the event (or handing a stream a nil channel) would
	// silently break delivery. The overshoot is bounded by the open streams.
	s := &apiEventSession{subscribers: make(map[*apiEventSubscriber]struct{})}
	l.sessions[id] = s
	l.touchLocked(s)
	return s
}

func (l *APIEventLog) touchLocked(s *apiEventSession) {
	l.useSeq++
	s.lastUse = l.useSeq
}

func cloneAPIEvent(event APIEvent) APIEvent {
	event.Data = append(json.RawMessage(nil), event.Data...)
	return event
}
