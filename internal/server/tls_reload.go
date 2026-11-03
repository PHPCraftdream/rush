package server

import (
	"context"
	"log/slog"
	"time"
)

type tlsReloadTicker interface {
	C() <-chan time.Time
	Stop()
}

type realTLSReloadTicker struct {
	ticker *time.Ticker
}

func (t realTLSReloadTicker) C() <-chan time.Time { return t.ticker.C }

func (t realTLSReloadTicker) Stop() { t.ticker.Stop() }

type tlsReloadOptions struct {
	interval  time.Duration
	now       func() time.Time
	newTicker func(time.Duration) tlsReloadTicker
}

func runTLSReload(ctx context.Context, store *tlsCertificateStore, options tlsReloadOptions) {
	if store == nil {
		return
	}
	if options.interval <= 0 {
		options.interval = time.Minute
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.newTicker == nil {
		options.newTicker = func(interval time.Duration) tlsReloadTicker {
			return realTLSReloadTicker{ticker: time.NewTicker(interval)}
		}
	}
	ticker := options.newTicker(options.interval)
	defer ticker.Stop()
	var watchedHash [32]byte
	warned := make(map[time.Duration]bool)
	expiredLogged := false
	checkExpiry := func(now time.Time) {
		snapshot := store.current.Load()
		if snapshot == nil || snapshot.cert == nil || snapshot.cert.Leaf == nil {
			return
		}
		if snapshot.certHash != watchedHash {
			watchedHash = snapshot.certHash
			warned = make(map[time.Duration]bool)
			expiredLogged = false
		}
		remaining := snapshot.cert.Leaf.NotAfter.Sub(now)
		for _, threshold := range []time.Duration{30 * 24 * time.Hour, 7 * 24 * time.Hour, 24 * time.Hour} {
			if remaining > 0 && remaining <= threshold && !warned[threshold] {
				slog.Warn("TLS certificate is approaching expiry", "expires_at", snapshot.cert.Leaf.NotAfter, "remaining", remaining)
				warned[threshold] = true
			}
		}
		if remaining <= 0 && !expiredLogged {
			slog.Error("TLS certificate has expired", "expires_at", snapshot.cert.Leaf.NotAfter)
			expiredLogged = true
		}
	}
	checkExpiry(options.now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if err := store.reloadAt(options.now()); err != nil {
				slog.Warn("TLS certificate reload failed; retaining last good certificate", "error", err)
			}
			checkExpiry(options.now())
		}
	}
}
