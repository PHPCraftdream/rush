package server

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

func sourceIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return remote
}

type rateEntry struct {
	tokens float64
	at     time.Time
}
type rateLimiter struct {
	mu           sync.Mutex
	entries      map[string]rateEntry
	rps          float64
	burst        float64
	clock        func() time.Time
	lastSweep    time.Time
	sweepStarted bool
}

func newRateLimiter(rps float64, burst int, clock func() time.Time) *rateLimiter {
	return &rateLimiter{entries: make(map[string]rateEntry), rps: rps, burst: float64(burst), clock: clock}
}

func (l *rateLimiter) Allow(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shouldSweep(now) {
		for k, e := range l.entries {
			if now.Sub(e.at) > time.Minute {
				delete(l.entries, k)
			}
		}
	}
	e, ok := l.entries[key]
	if ok && now.Sub(e.at) > time.Minute {
		delete(l.entries, key)
		ok = false
	}
	if !ok {
		e = rateEntry{tokens: l.burst, at: now}
	}
	elapsed := now.Sub(e.at).Seconds()
	if elapsed > 0 {
		e.tokens += elapsed * l.rps
		if e.tokens > l.burst {
			e.tokens = l.burst
		}
	}
	e.at = now
	if e.tokens < 1 {
		l.entries[key] = e
		wait := time.Duration((1 - e.tokens) / l.rps * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return wait
	}
	e.tokens--
	l.entries[key] = e
	return 0
}

func (l *rateLimiter) shouldSweep(now time.Time) bool {
	if l.sweepStarted && now.Sub(l.lastSweep) < time.Minute {
		return false
	}
	l.lastSweep = now
	l.sweepStarted = true
	return true
}

type failureEntry struct {
	times        []time.Time
	offences     int
	blockedUntil time.Time
	lastActivity time.Time
}
type failureBackoff struct {
	mu           sync.Mutex
	entries      map[string]failureEntry
	clock        func() time.Time
	lastSweep    time.Time
	sweepStarted bool
}

func newFailureBackoff(clock func() time.Time) *failureBackoff {
	return &failureBackoff{entries: make(map[string]failureEntry), clock: clock}
}

func (b *failureBackoff) Blocked(ip string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shouldSweep(now) {
		for k, e := range b.entries {
			if failureEntryExpired(e, now) {
				delete(b.entries, k)
			}
		}
	}
	e, ok := b.entries[ip]
	if ok && failureEntryExpired(e, now) {
		delete(b.entries, ip)
		ok = false
	}
	if !ok || !now.Before(e.blockedUntil) {
		return 0
	}
	return e.blockedUntil.Sub(now)
}

func (b *failureBackoff) shouldSweep(now time.Time) bool {
	if b.sweepStarted && now.Sub(b.lastSweep) < time.Minute {
		return false
	}
	b.lastSweep = now
	b.sweepStarted = true
	return true
}

func failureEntryExpired(e failureEntry, now time.Time) bool {
	if e.offences == 0 {
		return now.Sub(e.lastActivity) >= time.Minute
	}
	return !now.Before(e.blockedUntil.Add(backoffDelay(e.offences)))
}

func (b *failureBackoff) Failure(ip string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[ip]
	if e.offences == 0 {
		if !e.lastActivity.IsZero() && now.Sub(e.lastActivity) >= time.Minute {
			e = failureEntry{}
		}
	} else if !now.Before(e.blockedUntil.Add(backoffDelay(e.offences))) {
		e = failureEntry{}
	}
	kept := e.times[:0]
	for _, failedAt := range e.times {
		if now.Sub(failedAt) <= time.Minute {
			kept = append(kept, failedAt)
		}
	}
	e.times = append(kept, now)
	e.lastActivity = now
	if e.offences > 0 {
		e.offences++
		e.blockedUntil = now.Add(backoffDelay(e.offences))
		e.times = nil
	} else if len(e.times) >= 10 {
		e.offences = 1
		e.blockedUntil = now.Add(backoffDelay(1))
		e.times = nil
	}
	b.entries[ip] = e
}

func backoffDelay(offences int) time.Duration {
	if offences <= 0 {
		return time.Minute
	}
	d := time.Second * time.Duration(1<<min(offences-1, 20))
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}

type tokenSlots struct {
	mu     sync.Mutex
	max    int
	active map[string]int
}

func (s *tokenSlots) Acquire(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[id] >= s.max {
		return false
	}
	s.active[id]++
	return true
}

func (s *tokenSlots) Release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[id]--
	if s.active[id] <= 0 {
		delete(s.active, id)
	}
}

func bodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

func retryAfterSeconds(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	return strconv.Itoa(int((d + time.Second - 1) / time.Second))
}
