package server

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterEvictsStaleLookupWithoutSweep(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRateLimiter(0, 1, func() time.Time { return now })
	l.sweepStarted = true
	l.lastSweep = now
	l.entries["key"] = rateEntry{tokens: 0, at: now.Add(-time.Minute - time.Nanosecond)}
	if wait := l.Allow("key", now); wait != 0 {
		t.Fatalf("stale entry limited request for %s", wait)
	}
	if got := l.entries["key"].tokens; got != 0 {
		t.Fatalf("tokens after absent-entry request = %v, want 0", got)
	}
}

func TestFailureBackoffEvictsStaleLookupWithoutSweep(t *testing.T) {
	now := time.Unix(2000, 0)
	b := newFailureBackoff(func() time.Time { return now })
	b.sweepStarted = true
	b.lastSweep = now
	b.entries["ip"] = failureEntry{offences: 1, blockedUntil: now.Add(-time.Minute)}
	if got := b.Blocked("ip", now); got != 0 {
		t.Fatalf("stale entry blocked for %s", got)
	}
	if _, ok := b.entries["ip"]; ok {
		t.Fatal("stale looked-up entry was not removed")
	}
}

func TestRateLimiterSweepIsPeriodic(t *testing.T) {
	now := time.Unix(3000, 0)
	l := newRateLimiter(1, 1, func() time.Time { return now })
	for i := 0; i < 20; i++ {
		l.Allow(fmt.Sprint(i), now)
	}
	for i := 0; i < 20; i++ {
		l.entries[fmt.Sprint("stale", i)] = rateEntry{tokens: 1, at: now.Add(-2 * time.Minute)}
	}
	for i := 0; i < 20; i++ {
		l.Allow(fmt.Sprint("missing", i), now)
	}
	if len(l.entries) != 60 {
		t.Fatalf("entries before interval sweep = %d, want 60", len(l.entries))
	}
	now = now.Add(time.Minute)
	l.Allow("next", now)
	if len(l.entries) != 41 {
		t.Fatalf("entries after interval sweep = %d, want 41", len(l.entries))
	}
}

func TestFailureBackoffSweepIsPeriodic(t *testing.T) {
	now := time.Unix(4000, 0)
	b := newFailureBackoff(func() time.Time { return now })
	for i := 0; i < 20; i++ {
		b.entries[fmt.Sprint(i)] = failureEntry{lastActivity: now}
	}
	b.Blocked("seed", now)
	for i := 0; i < 20; i++ {
		b.entries[fmt.Sprint("stale", i)] = failureEntry{lastActivity: now.Add(-time.Minute)}
	}
	for i := 0; i < 20; i++ {
		b.Blocked(fmt.Sprint("missing", i), now)
	}
	if len(b.entries) != 40 {
		t.Fatalf("entries before interval sweep = %d, want 40", len(b.entries))
	}
	now = now.Add(time.Minute)
	b.Blocked("next", now)
	if len(b.entries) != 0 {
		t.Fatalf("entries after interval sweep = %d, want 0", len(b.entries))
	}
}

func TestRateLimiterEvictionBoundary(t *testing.T) {
	now := time.Unix(5000, 0)
	l := newRateLimiter(0.0001, 1, func() time.Time { return now })
	l.sweepStarted = true
	l.lastSweep = now
	l.entries["exact"] = rateEntry{tokens: 0, at: now.Add(-time.Minute)}
	if wait := l.Allow("exact", now); wait < time.Second {
		t.Fatalf("exactly one-minute-old entry was evicted; wait = %s", wait)
	}
	l.entries["older"] = rateEntry{tokens: 0, at: now.Add(-time.Minute - time.Nanosecond)}
	if wait := l.Allow("older", now); wait != 0 {
		t.Fatalf("entry older than one minute was retained; wait = %s", wait)
	}
}

func TestFailureBackoffEvictionBoundary(t *testing.T) {
	now := time.Unix(6000, 0)
	b := newFailureBackoff(func() time.Time { return now })
	b.sweepStarted = true
	b.lastSweep = now
	b.entries["exact"] = failureEntry{lastActivity: now.Add(-time.Minute)}
	b.Blocked("exact", now)
	if _, ok := b.entries["exact"]; ok {
		t.Fatal("exactly one-minute-idle entry was retained")
	}
	b.entries["active"] = failureEntry{offences: 1, blockedUntil: now.Add(-time.Millisecond)}
	b.Blocked("active", now)
	if _, ok := b.entries["active"]; !ok {
		t.Fatal("entry before blockedUntil plus delay was evicted")
	}
	b.entries["expired"] = failureEntry{offences: 1, blockedUntil: now.Add(-backoffDelay(1))}
	b.Blocked("expired", now)
	if _, ok := b.entries["expired"]; ok {
		t.Fatal("entry at blockedUntil plus delay was retained")
	}
}
