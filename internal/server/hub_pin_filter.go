package server

import "encoding/json"

type eventClass uint8

const (
	eventUnknown eventClass = iota
	eventNeverPinned
	eventSession
	eventSafeGlobal
	eventSensitiveGlobal
)

type pinnedView struct{ sessionID string }

func newPinnedView(sessionID string) pinnedView { return pinnedView{sessionID: sessionID} }

// eventClasses explicitly classifies every outbound protocol event. Globals
// not explicitly safe for pinned views are denied, including sensitive globals.
var eventClasses = map[string]eventClass{
	EventHello:          eventNeverPinned, // hello is emitted directly by handleWS, not the hub.
	EventMessageCreated: eventSession, EventMessageUpdated: eventSession,
	EventMessageDeleted: eventSession, EventSessionCreated: eventSession,
	EventSessionUpdated: eventSession, EventSessionDeleted: eventSession,
	EventMCPState: eventSensitiveGlobal, EventLSPState: eventNeverPinned,
	EventAgentBusy: eventSession, EventSessionsList: eventSession,
	EventMessagesList: eventSession, EventConfig: eventSensitiveGlobal,
	EventLogs: eventSensitiveGlobal, EventUpdateAvailable: eventSafeGlobal,
	EventResponse: eventSession, EventError: eventNeverPinned,
	EventSystemPrompt: eventSession, EventSkills: eventNeverPinned,
	EventSummarizeQueued: eventSession, EventScopedModels: eventNeverPinned,
	EventSessionLiveWork: eventSession, EventSessionWakeSchedules: eventSession,
	EventSessionBatchWork: eventSession, EventBatchControlResult: eventSession,
}

type eventMetadata struct {
	msgType    string
	class      eventClass
	sessionIDs []string
	valid      bool
}

type eventSessionRef struct {
	SessionID string `json:"sessionID"`
	Session   string `json:"session"`
	ID        string `json:"id"`
}

func (r eventSessionRef) sessionID() string {
	if r.SessionID != "" {
		return r.SessionID
	}
	if r.Session != "" {
		return r.Session
	}
	return r.ID
}

// lazyEventMeta defers the payload parse: most deliveries reach no pinned
// client, and message deltas are frequent and large.
type lazyEventMeta struct {
	raw      []byte
	msgType  string
	typeDone bool
	full     eventMetadata
	fullDone bool
}

func newLazyMeta(raw []byte) *lazyEventMeta { return &lazyEventMeta{raw: raw} }

func (m *lazyEventMeta) typ() string {
	if !m.typeDone {
		m.msgType, m.typeDone = envelopeType(m.raw), true
	}
	return m.msgType
}

func (m *lazyEventMeta) meta() eventMetadata {
	if !m.fullDone {
		m.full, m.fullDone = classifyEnvelope(m.raw), true
	}
	return m.full
}

// classifyEnvelope derives type and session ownership once for each hub
// delivery. envelopeType keeps the common Broadcast encoding fast-path.
func classifyEnvelope(raw []byte) eventMetadata {
	meta := eventMetadata{msgType: envelopeType(raw), valid: true}
	meta.class = eventClasses[meta.msgType]
	if meta.class != eventSession {
		return meta
	}
	var message struct {
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(raw, &message) != nil {
		meta.valid = false
		return meta
	}
	var items []eventSessionRef
	if len(message.Payload) > 0 && message.Payload[0] == '[' {
		if json.Unmarshal(message.Payload, &items) != nil {
			meta.valid = false
			return meta
		}
	} else {
		var item eventSessionRef
		if len(message.Payload) == 0 || json.Unmarshal(message.Payload, &item) != nil {
			meta.valid = false
			return meta
		}
		items = []eventSessionRef{item}
	}
	meta.sessionIDs = make([]string, len(items))
	for i, item := range items {
		meta.sessionIDs[i] = item.sessionID()
		if meta.sessionIDs[i] == "" {
			meta.valid = false
		}
	}
	return meta
}

func (v pinnedView) allows(meta eventMetadata) bool {
	if v.sessionID == "" {
		return true
	}
	if !meta.valid {
		return false
	}
	switch meta.class {
	case eventSafeGlobal:
		return meta.msgType == EventUpdateAvailable
	case eventSession:
		if len(meta.sessionIDs) == 0 {
			return false
		}
		for _, id := range meta.sessionIDs {
			if id != v.sessionID {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (v pinnedView) project(raw []byte, meta eventMetadata) []byte {
	if !v.allows(meta) {
		return nil
	}
	if meta.class != eventSafeGlobal {
		return raw
	}
	var message WSMessage
	if json.Unmarshal(raw, &message) != nil {
		return nil
	}
	var update struct {
		Current string `json:"current"`
		Latest  string `json:"latest"`
	}
	if len(message.Payload) == 0 || json.Unmarshal(message.Payload, &update) != nil {
		return nil
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return nil
	}
	message.Payload = payload
	projected, err := json.Marshal(message)
	if err != nil {
		return nil
	}
	return projected
}

func isSessionEventType(msgType string) bool { return eventClasses[msgType] == eventSession }
