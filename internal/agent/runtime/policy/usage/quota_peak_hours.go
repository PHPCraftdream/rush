package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config"
)

var errProviderPeakHours = errors.New("provider is inside its configured peak-hours window")

// PeakHoursSentinel returns the stable private error-class marker for adapters.
func PeakHoursSentinel() error { return errProviderPeakHours }

type PeakHoursError struct {
	ProviderID string
	Start, End string // HH:MM, as configured
	ReopensAt  time.Time
	Message    string
	Timezone   string
}

func (e *PeakHoursError) Error() string {
	if e.ReopensAt.IsZero() {
		return fmt.Sprintf(
			"provider %s is in peak hours (%s–%s), refusing with no scheduled reopen (blocked around the clock)",
			e.ProviderID, e.Start, e.End,
		)
	}
	return fmt.Sprintf(
		"provider %s is in peak hours (%s–%s), refusing until %s",
		e.ProviderID, e.Start, e.End, e.ReopensAt.Format("15:04"),
	)
}

func (e *PeakHoursError) Unwrap() error { return errProviderPeakHours }

func CheckPeakHours(providerCfg config.ProviderConfig) error {
	return checkPeakHours(providerCfg)
}

func checkPeakHours(providerCfg config.ProviderConfig) error {
	ph := providerCfg.PeakHours
	if ph == nil || ph.IsOff() {
		return nil
	}
	now := time.Now()
	st := ph.Status(now)
	if !st.Blocked {
		return nil
	}
	e := &PeakHoursError{
		ProviderID: providerCfg.ID,
		Message:    ph.EffectiveMessage(),
		Timezone:   st.Timezone,
	}
	e.ReopensAt = st.ReopensAt
	switch {
	case ph.Legacy != nil:
		e.Start, e.End = ph.Legacy.Start, ph.Legacy.End
		if st.InPeak {
			e.ReopensAt = ph.Legacy.EndTimeToday(now)
		}
	case st.InPeak && !st.BandStart.IsZero():
		loc := time.Local
		if tz, err := config.ResolvePeakTimezone(st.Timezone); err == nil && tz != nil {
			loc = tz
		}
		e.Start = st.BandStart.In(loc).Format("15:04")
		if !st.ReopensAt.IsZero() {
			e.End = st.ReopensAt.In(loc).Format("15:04")
		}
	}
	msg := "Refusing request: provider is inside its peak-hours window"
	args := []any{"provider", providerCfg.ID, "window_start", e.Start, "window_end", e.End}
	if !e.ReopensAt.IsZero() {
		args = append(args,
			"available_again", e.ReopensAt.Format("15:04"),
			"in", time.Until(e.ReopensAt).Round(time.Minute).String(),
		)
	}
	if st.Timezone != "" {
		args = append(args, "timezone", st.Timezone)
	}
	slog.Warn(msg, args...)
	return e
}

// PeakHoursGuidance returns the orchestrator-facing instruction paragraph
// for ANY peak-hours refusal — the "refused at the start of a new
// invocation" case (checkPeakHours' pre-flight call sites). The gate is
// pre-flight only (task #56): a window opening mid-turn never aborts a
// running stream. The refusal must carry the same guidance: this is not a
// crash, rush will not retry on its own, and an orchestrating agent
// should schedule a resume for the exact reopen time rather than retry
// immediately.
//
// This is the single source of truth for the guidance text — both app.go
// (stderr output before fang's ERROR box) and peakHoursStoppedFinishText
// (DB finish-message details) call it, so the operator-visible stderr
// text and the recorded DB details stay identical.
//
// err is whatever checkPeakHours returned. When it is (or wraps) a
// *PeakHoursError we pull the exact ReopensAt time.Time out of it — via
// errors.As, not string parsing — so the instruction gives an
// orchestrating agent an unambiguous, cron-ready timestamp instead of
// making it parse "refusing until 12:00" out of prose. Falls back to a
// generic RESUME AT line if the type assertion fails (defensive: keeps
// working even if the error ever gets wrapped differently upstream).
func PeakHoursGuidance(err error) string {
	resumeLine := "RESUME AT: (see the time above)"
	var pe *PeakHoursError
	if errors.As(err, &pe) {
		if pe.ReopensAt.IsZero() {
			resumeLine = "RESUME AT: unknown — the provider is blocked around the clock; do not schedule a timed resume."
		} else {
			resumeLine = fmt.Sprintf(
				"RESUME AT: %s (local time, RFC3339: %s)",
				pe.ReopensAt.Format("2006-01-02 15:04"),
				pe.ReopensAt.Format(time.RFC3339),
			)
		}
		// The configured timezone is appended only when one is set, so
		// legacy-shape output (no timezone configured) stays byte-identical.
		if pe.Timezone != "" {
			resumeLine += fmt.Sprintf(" (timezone: %s)", pe.Timezone)
		}
	}
	guidance := fmt.Sprintf(
		"%s\n\n"+
			"This is not a crash — rush is intentionally refusing/halting because "+
			"of the provider's peak-hours window. rush is exiting now; it will "+
			"not retry on its own.\n\n"+
			"If an orchestrating agent is driving this session: schedule a resume "+
			"for RESUME AT above (e.g. CronCreate / a scheduled wakeup) and "+
			"re-invoke `rush run --session <this session>` then — do not retry "+
			"immediately, and do not pass --allow-peak-hours unless a human "+
			"operator explicitly asked for that specific run.",
		resumeLine,
	)
	// Operator-authored custom message (config.PeakHoursWindow.Message),
	// appended after a blank-line separator, exactly as with the sections
	// above — empty by default, so this is a no-op unless configured.
	if pe != nil && pe.Message != "" {
		guidance += "\n\n" + pe.Message
	}
	return guidance
}

// peakHoursStoppedFinishText builds the (msg, details) pair recorded as
// the Finish part when a pre-flight peak-hours refusal renders through
// the turn-failure path (agent_turn_failure.go). The gate is pre-flight
// only (task #56): this renders a refusal that blocked the request before
// it reached the provider.
func PeakHoursStoppedFinishText(err error) (msg, details string) {
	msg = "Stopped: provider entered its peak-hours window mid-session"
	details = fmt.Sprintf("%s\n\n%s", err.Error(), PeakHoursGuidance(err))
	return msg, details
}

func IsHardQuotaLimit(err error) bool {
	return classifyHardProviderLimit(err, "", time.Now()) == providerLimitHard
}

func IsQuotaLimit(providerErr *fantasy.ProviderError) bool {
	return isQuotaLimit(providerErr)
}

func isQuotaLimit(providerErr *fantasy.ProviderError) bool {
	return classifyHardProviderLimit(providerErr, "", time.Now()) == providerLimitHard
}

type LegacyPeakHoursBypassKey struct{}

type peakHoursRequestProvider struct {
	fantasy.Provider
	check func(context.Context) error
}

func (p *peakHoursRequestProvider) LanguageModel(ctx context.Context, id string) (fantasy.LanguageModel, error) {
	model, err := p.Provider.LanguageModel(ctx, id)
	if err != nil {
		return nil, err
	}
	return &peakHoursRequestModel{LanguageModel: model, check: p.check}, nil
}

type PeakHoursRequestGateHost interface {
	CheckLivePeakHours(providerID string) error
	CallOptionsFrom(context.Context) *agentcall.CallOptions
}

type PeakHoursRequestGateComponent struct {
	host PeakHoursRequestGateHost
}

func NewPeakHoursRequestGateComponent(host PeakHoursRequestGateHost) *PeakHoursRequestGateComponent {
	return &PeakHoursRequestGateComponent{host: host}
}

func (g *PeakHoursRequestGateComponent) WithPeakHoursRequestGate(provider fantasy.Provider, pc config.ProviderConfig) fantasy.Provider {
	return &peakHoursRequestProvider{Provider: provider, check: func(ctx context.Context) error {
		if opts := g.host.CallOptionsFrom(ctx); opts != nil {
			if opts.AllowPeakHours {
				return nil
			}
		} else if allowed, _ := ctx.Value(LegacyPeakHoursBypassKey{}).(bool); allowed {
			return nil
		}
		if pc.ID == "" {
			return checkPeakHours(pc)
		}
		return g.host.CheckLivePeakHours(pc.ID)
	}}
}

type peakHoursRequestModel struct {
	fantasy.LanguageModel
	check func(context.Context) error
}

func (m *peakHoursRequestModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.Generate(ctx, call)
}

func (m *peakHoursRequestModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.Stream(ctx, call)
}

func (m *peakHoursRequestModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.GenerateObject(ctx, call)
}

func (m *peakHoursRequestModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.StreamObject(ctx, call)
}

// NewPeakHoursRequestModel wraps lm so every call first passes check (test seam).
func NewPeakHoursRequestModel(lm fantasy.LanguageModel, check func(context.Context) error) fantasy.LanguageModel {
	return &peakHoursRequestModel{LanguageModel: lm, check: check}
}
