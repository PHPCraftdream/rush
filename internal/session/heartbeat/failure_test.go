package heartbeat

// Revert-check map (test -> the single-line production change it must catch):
//   TestRecordFailureCountsErrorsWithoutRequests -> RecordFailure's Errors/Totals increments (and its "no Requests" contract)
//   TestRecordFailureClassifiesLimitHits         -> the isLimit gate inside RecordFailure
//   TestRecordFailureRedactsSecrets              -> RedactError applied to LastError in RecordFailure
//   TestRecordFailureCreatesUnknownModel         -> getModel's lazy entry creation reached from RecordFailure
//   TestRecordFailureNilErrorNoop                -> RecordFailure's nil-error early return

import (
	"errors"
	"strings"
	"testing"
)

func TestRecordFailureCountsErrorsWithoutRequests(t *testing.T) {
	setup(t)
	ctx := withCtx(Context{SessionID: "fail-1"})
	RecordFailure(ctx, "p", "m", errors.New("boom one"))
	RecordFailure(ctx, "p", "m", errors.New("boom two"))
	registry.Lock()
	r := registry.rows["fail-1"]
	registry.Unlock()
	if r == nil {
		t.Fatal("no row created")
	}
	m := r.Models["p/m"]
	if m == nil {
		t.Fatal("model entry missing")
	}
	if m.Errors != 2 || r.Totals.Errors != 2 {
		t.Fatalf("errors model=%d totals=%d, want 2/2", m.Errors, r.Totals.Errors)
	}
	if m.Requests != 0 || r.Totals.Requests != 0 {
		t.Fatalf("requests model=%d totals=%d, must stay 0", m.Requests, r.Totals.Requests)
	}
}

func TestRecordFailureClassifiesLimitHits(t *testing.T) {
	setup(t)
	RecordFailure(withCtx(Context{SessionID: "fail-2"}), "p", "m", errors.New("request failed: HTTP 429 Too Many Requests"))
	RecordFailure(withCtx(Context{SessionID: "fail-2"}), "p", "m", errors.New("plain network error"))
	registry.Lock()
	r := registry.rows["fail-2"]
	registry.Unlock()
	m := r.Models["p/m"]
	if m.LimitHits != 1 || r.Totals.LimitHits != 1 {
		t.Fatalf("limit hits model=%d totals=%d, want 1/1", m.LimitHits, r.Totals.LimitHits)
	}
	if m.Errors != 2 || r.Totals.Errors != 2 {
		t.Fatalf("errors model=%d totals=%d, want 2/2", m.Errors, r.Totals.Errors)
	}
}

func TestRecordFailureRedactsSecrets(t *testing.T) {
	setup(t)
	secret := "sk-supersecret123456789012"
	RecordFailure(withCtx(Context{SessionID: "fail-3"}), "p", "m", errors.New("auth failed: "+secret))
	registry.Lock()
	r := registry.rows["fail-3"]
	registry.Unlock()
	m := r.Models["p/m"]
	if strings.Contains(m.LastError, secret) {
		t.Fatalf("secret persisted in LastError: %q", m.LastError)
	}
	if m.LastError == "" {
		t.Fatal("LastError empty")
	}
	if r.Totals.Errors != 1 {
		t.Fatalf("totals errors=%d", r.Totals.Errors)
	}
}

func TestRecordFailureCreatesUnknownModel(t *testing.T) {
	setup(t)
	RecordFailure(withCtx(Context{SessionID: "fail-4"}), "newp", "newm", errors.New("first failure"))
	registry.Lock()
	r := registry.rows["fail-4"]
	registry.Unlock()
	if r == nil {
		t.Fatal("no row created for unknown model")
	}
	m := r.Models["newp/newm"]
	if m == nil {
		t.Fatal("unknown model entry not created")
	}
	if m.Provider != "newp" || m.Model != "newm" {
		t.Fatalf("identity=%s/%s", m.Provider, m.Model)
	}
}

func TestRecordFailureNilErrorNoop(t *testing.T) {
	setup(t)
	RecordFailure(withCtx(Context{SessionID: "fail-5"}), "p", "m", nil)
	registry.Lock()
	_, ok := registry.rows["fail-5"]
	registry.Unlock()
	if ok {
		t.Fatal("nil error must not create a row")
	}
}
