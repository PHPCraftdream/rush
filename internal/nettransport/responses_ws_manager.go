package nettransport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ResponsesWSManager owns sequential connections; busy leases are never shared.
// Its owner-bound sweeper retires idle or revoked connections, never active
// exchanges solely because they reached the planned reuse age.
type ResponsesWSManager struct {
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	closed      bool
	limit       int
	now         func() time.Time
	occupancy   int
	done        chan struct{}
	dialWG      sync.WaitGroup
	entries     map[[32]byte]*responsesWSEntry
	unsupported map[[32]byte]time.Time
}

type responsesWSEntry struct {
	conn         *ResponsesWSConn
	conversation string
	protocol     string
	busy         bool
	created      time.Time
	used         time.Time
	maxAge       time.Duration
	cancel       context.CancelFunc
	valid        func() bool
	reuse        bool
}

type ResponsesWSLease struct {
	manager *ResponsesWSManager
	key     [32]byte
	entry   *responsesWSEntry
	once    sync.Once
}

const ResponsesWSPlannedMaxAge = 20 * time.Minute

func NewResponsesWSManager(parent context.Context, limit int) *ResponsesWSManager {
	if limit <= 0 {
		limit = 16
	}
	limit = min(limit, 128)
	ctx, cancel := context.WithCancel(parent)
	m := &ResponsesWSManager{ctx: ctx, cancel: cancel, limit: limit, now: time.Now,
		entries: make(map[[32]byte]*responsesWSEntry), unsupported: make(map[[32]byte]time.Time), done: make(chan struct{})}
	go m.maintain()
	return m
}

// Acquire reserves before dialing so concurrent handshakes count toward the bound.
// maxAge is a local reuse budget, not a claim about the server's lifetime.
func (m *ResponsesWSManager) Acquire(ctx context.Context, options ResponsesWSOptions, maxAge time.Duration) (*ResponsesWSLease, error) {
	if err := validateResponsesWSOptions(&options); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, errors.New("responses websocket: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxAge <= 0 || maxAge > ResponsesWSPlannedMaxAge {
		maxAge = ResponsesWSPlannedMaxAge
	}
	key, err := responsesWSConnectionKey(options)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	now := m.now()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return nil, errors.New("responses websocket: manager closed")
	}
	m.sweepLocked(now)
	if now.Before(m.unsupported[key]) {
		m.mu.Unlock()
		return nil, errors.New("responses websocket: endpoint capability unavailable")
	}
	if e := m.entries[key]; e != nil {
		if e.busy {
			m.mu.Unlock()
			return nil, errors.New("responses websocket: conversation connection busy")
		}
		e.busy = true
		m.mu.Unlock()
		return &ResponsesWSLease{manager: m, key: key, entry: e}, nil
	}
	// A credential or network change retires the previous conversation connection.
	for k, e := range m.entries {
		if e.conversation == options.Owner.ConversationID && e.protocol == options.Protocol {
			if e.busy {
				m.mu.Unlock()
				return nil, errors.New("responses websocket: conversation connection busy")
			}
			m.removeLocked(k, e)
		}
	}
	if m.occupancy >= m.limit {
		var oldestKey [32]byte
		var oldest *responsesWSEntry
		for k, e := range m.entries {
			if !e.busy && (oldest == nil || e.used.Before(oldest.used)) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			m.mu.Unlock()
			return nil, errors.New("responses websocket: connection capacity reached")
		}
		m.removeLocked(oldestKey, oldest)
	}
	dialCtx, cancel := context.WithCancel(ctx)
	e := &responsesWSEntry{conversation: options.Owner.ConversationID, protocol: options.Protocol, busy: true, created: now, used: now, maxAge: maxAge, cancel: cancel, valid: options.StillCurrent, reuse: options.TLSClientConfig == nil}
	m.entries[key] = e
	m.occupancy++
	m.dialWG.Add(1)
	m.mu.Unlock()
	defer m.dialWG.Done()

	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel() }()
	conn, dialErr := DialResponsesWebSocket(dialCtx, options)
	m.mu.Lock()
	defer m.mu.Unlock()
	if dialErr != nil {
		m.occupancy--
		if m.entries[key] == e {
			delete(m.entries, key)
		}
		var handshake *ResponsesWSHandshakeError
		if errors.As(dialErr, &handshake) && (handshake.Status == http.StatusNotFound || handshake.Status == http.StatusMethodNotAllowed || handshake.Status == http.StatusUpgradeRequired) && !m.closed {
			if len(m.unsupported) >= m.limit {
				for k := range m.unsupported {
					delete(m.unsupported, k)
					break
				}
			}
			m.unsupported[key] = m.now().Add(5 * time.Minute)
		}
		return nil, dialErr
	}
	if m.closed || m.entries[key] != e || ctx.Err() != nil {
		m.occupancy--
		_ = conn.Close()
		if m.entries[key] == e {
			delete(m.entries, key)
		}
		return nil, errors.New("responses websocket: lease invalidated during handshake")
	}
	e.conn = conn
	return &ResponsesWSLease{manager: m, key: key, entry: e}, nil
}

func responsesWSConnectionKey(o ResponsesWSOptions) ([32]byte, error) {
	// Hash credentials and policy, never retain them in map keys or diagnostics.
	value := struct {
		Protocol, Endpoint, Token string
		Owner                     ResponsesWSOwner
		Auth, Headers             http.Header
		Network                   any
		TLS                       any
		Handshake, IO             time.Duration
		Frame, Exchange           int64
		ReadPolicy                ResponsesWSReadPolicy
	}{o.Protocol, o.Endpoint, o.AuthToken, o.Owner, o.AuthHeaders, o.Headers, o.Network, nil, o.HandshakeTimeout, o.IOTimeout, o.MaxFrameBytes, o.MaxExchangeBytes, o.ReadPolicy}
	// TLS configurations cannot be compared safely. Connections with custom TLS
	// are not pooled; the handshake still uses a clone of the supplied policy.
	if o.TLSClientConfig != nil {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return [32]byte{}, errors.New("responses websocket: unable to isolate TLS policy")
		}
		value.TLS = nonce
	}
	data, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, errors.New("responses websocket: invalid connection identity")
	}
	return sha256.Sum256(data), nil
}

func (l *ResponsesWSLease) Conn() *ResponsesWSConn { return l.entry.conn }

// Release retains only an accepted terminal exchange. All other paths retire it.
func (l *ResponsesWSLease) Release() {
	l.once.Do(func() {
		m := l.manager
		m.mu.Lock()
		defer m.mu.Unlock()
		e := l.entry
		if m.entries[l.key] != e {
			return
		}
		e.busy = false
		e.used = m.now()
		if m.closed || !e.reuse || e.conn == nil || !e.conn.Reusable() || e.used.Sub(e.created) >= e.maxAge {
			m.removeLocked(l.key, e)
		}
	})
}

func (m *ResponsesWSManager) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed || m.ctx.Err() != nil
}

func (m *ResponsesWSManager) InvalidateConversation(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.entries {
		if e.conversation == id {
			m.removeLocked(key, e)
		}
	}
}

func (m *ResponsesWSManager) removeLocked(key [32]byte, e *responsesWSEntry) {
	delete(m.entries, key)
	if e.cancel != nil {
		e.cancel()
	}
	if e.conn != nil {
		slog.Debug("responses websocket retired", "protocol", e.protocol, "age_seconds", max(0, m.now().Sub(e.created).Seconds()), "planned_seconds", e.maxAge.Seconds(), "send_state", e.conn.State().String(), "learning_reason", "not_enabled")
		_ = e.conn.Close()
		m.occupancy--
	}
}

func (m *ResponsesWSManager) maintain() {
	defer close(m.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			m.shutdown()
			return
		case <-ticker.C:
			m.mu.Lock()
			m.sweepLocked(m.now())
			m.mu.Unlock()
		}
	}
}

func (m *ResponsesWSManager) sweepLocked(now time.Time) {
	for key, expiry := range m.unsupported {
		if !now.Before(expiry) {
			delete(m.unsupported, key)
		}
	}
	for key, e := range m.entries {
		revoked := e.valid != nil && !e.valid()
		if revoked || (!e.busy && (now.Sub(e.used) >= 90*time.Second || now.Sub(e.created) >= e.maxAge || e.conn == nil || !e.conn.Reusable())) {
			m.removeLocked(key, e)
		}
	}
}

func (m *ResponsesWSManager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	m.cancel()
	for key, e := range m.entries {
		m.removeLocked(key, e)
	}
	clear(m.unsupported)
}

func (m *ResponsesWSManager) Close() error {
	m.shutdown()
	<-m.done
	m.dialWG.Wait()
	return nil
}
