// Time source for the run-queue pump's lease logic.

package session

import "time"

// PumpClock is the time source the pump's lease, renewal, watchdog, expiry
// and busy-backoff logic reads. Production always uses the real clock
// (RunQueuePumpConfig.TestClock == nil); a test injects a fake to advance
// across many TTL windows without any wall-clock scheduling dependence.
type PumpClock interface {
	Now() time.Time
	NewTicker(d time.Duration) PumpTicker
}

// PumpTicker is the subset of *time.Ticker the pump uses.
type PumpTicker interface {
	C() <-chan time.Time
	Stop()
}

type realPumpClock struct{}

func (realPumpClock) Now() time.Time { return time.Now() }

func (realPumpClock) NewTicker(d time.Duration) PumpTicker {
	return realPumpTicker{t: time.NewTicker(d)}
}

type realPumpTicker struct{ t *time.Ticker }

func (r realPumpTicker) C() <-chan time.Time { return r.t.C }
func (r realPumpTicker) Stop()               { r.t.Stop() }

// clock returns the injected clock, or the real one.
func (p *RunQueuePump) clock() PumpClock {
	if p.cfg.TestClock != nil {
		return p.cfg.TestClock
	}
	return realPumpClock{}
}

// now reads the pump's clock.
func (p *RunQueuePump) now() time.Time { return p.clock().Now() }
