package agent

// Revert-check map (test -> the single-line production change it must catch):
//   TestHbDecoratorRecordsFourCallKinds     -> a RecordRequest line dropped from one hbLanguageModel method
//   TestHbDecoratorPassthrough              -> a response/error rewritten instead of returned verbatim
//   TestHbStreamPartsPassThroughInOrder     -> observeStream dropping/reordering parts
//   TestHbStreamErrorPartRecordsOneFailure  -> RecordFailure removed from observeStream's error branch
//   TestHbStreamEarlyStopRecordsNoFailure   -> failure recorded eagerly in Stream instead of inside the iterator
//   TestHbRecordsWrappedModelIdentity       -> identity() reading config ids instead of inner.Provider/Model
//   TestHbRedactsBearerTokenInError         -> error text recorded without heartbeat's RedactError
//   TestHbContextWithoutAttributionUsesNone -> rootID's "_none" fallback
//   TestHbConcurrentRecording               -> unsynchronized per-call recording state
//   TestHbIdempotentWrap                    -> NewHeartbeatProvider double-wrap guard
//   TestHbProviderLanguageModelDelegates    -> hbProvider.LanguageModel dropping the wrapper/error, or RecordRequest leaking outside the four call kinds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/stretchr/testify/require"
)

// hbIsolate points heartbeat at throwaway dirs for the duration of the test.
func hbIsolate(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", d)
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Cleanup(func() { heartbeat.Shutdown(time.Second) })
	return d
}

// hbFakeModel is a fantasy.LanguageModel double with scripted behavior.
type hbFakeModel struct {
	provider, model string
	resp            *fantasy.Response
	genErr          error
	parts           []fantasy.StreamPart
	streamErr       error
	genCalls        atomic.Int64
	streamCalls     atomic.Int64
	objCalls        atomic.Int64
}

func (m *hbFakeModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	m.genCalls.Add(1)
	if m.genErr != nil {
		return nil, m.genErr
	}
	return m.resp, nil
}

func (m *hbFakeModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.streamCalls.Add(1)
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	parts := m.parts
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *hbFakeModel) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.objCalls.Add(1)
	if m.genErr != nil {
		return nil, m.genErr
	}
	return &fantasy.ObjectResponse{}, nil
}

func (m *hbFakeModel) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.objCalls.Add(1)
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {}, nil
}

func (m *hbFakeModel) Provider() string { return m.provider }

func (m *hbFakeModel) Model() string { return m.model }

type hbFakeProvider struct {
	m   fantasy.LanguageModel
	err error
}

func (p *hbFakeProvider) Name() string { return "hb-fake" }

func (p *hbFakeProvider) LanguageModel(_ context.Context, _ string) (fantasy.LanguageModel, error) {
	return p.m, p.err
}

// hbAttributed returns a ctx attributed to a unique heartbeat session.
func hbAttributed(session string) context.Context {
	return heartbeat.WithContext(context.Background(), heartbeat.Context{
		SessionID: session, AgentID: session + "-agent",
		Purpose: heartbeat.PurposeTurn, Role: "smart", Source: "test",
	})
}

// hbShutdownFlush forces a bounded flush and returns the decoded snapshots.
func hbShutdownFlush(t *testing.T) []heartbeat.Entry {
	t.Helper()
	heartbeat.Shutdown(2 * time.Second)
	entries, err := heartbeat.ReadAll()
	require.NoError(t, err)
	return entries
}

func hbFindEntry(t *testing.T, entries []heartbeat.Entry, session string) *heartbeat.Entry {
	t.Helper()
	for i := range entries {
		if entries[i].Session == session {
			return &entries[i]
		}
	}
	t.Fatalf("no heartbeat entry for session %q in %d entries", session, len(entries))
	return nil
}

func TestHbDecoratorRecordsFourCallKinds(t *testing.T) {
	hbIsolate(t)
	m := &hbFakeModel{provider: "prov", model: "hbm", resp: &fantasy.Response{}}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-four"), "config-id")
	require.NoError(t, err)
	ctx := hbAttributed("hb-four")
	_, err = lm.Generate(ctx, fantasy.Call{})
	require.NoError(t, err)
	s, err := lm.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	for range s {
	}
	_, err = lm.GenerateObject(ctx, fantasy.ObjectCall{})
	require.NoError(t, err)
	so, err := lm.StreamObject(ctx, fantasy.ObjectCall{})
	require.NoError(t, err)
	for range so {
	}
	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-four")
	mm := entry.Models["prov/hbm"]
	require.NotNil(t, mm, "model prov/hbm not recorded")
	require.Equal(t, int64(4), mm.Requests)
	require.Equal(t, int64(0), mm.Errors)
	require.Equal(t, int64(4), mm.ByPurpose["turn"].Requests)
	require.Equal(t, int64(4), entry.Totals.Requests)
}

func TestHbDecoratorPassthrough(t *testing.T) {
	hbIsolate(t)
	sentinelGen := errors.New("hb-gen-boom")
	sentinelStream := errors.New("hb-stream-boom")
	m := &hbFakeModel{provider: "prov", model: "hbm", genErr: sentinelGen, streamErr: sentinelStream}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-pass"), "config-id")
	require.NoError(t, err)
	ctx := hbAttributed("hb-pass")
	resp, err := lm.Generate(ctx, fantasy.Call{})
	require.Nil(t, resp)
	require.ErrorIs(t, err, sentinelGen)
	_, err = lm.Stream(ctx, fantasy.Call{})
	require.ErrorIs(t, err, sentinelStream)
	oResp, err := lm.GenerateObject(ctx, fantasy.ObjectCall{})
	require.Nil(t, oResp)
	require.ErrorIs(t, err, sentinelGen)
	_, err = lm.StreamObject(ctx, fantasy.ObjectCall{})
	require.ErrorIs(t, err, sentinelStream)
	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-pass")
	mm := entry.Models["prov/hbm"]
	require.NotNil(t, mm)
	require.Equal(t, int64(4), mm.Requests, "request counted even when the call fails immediately")
	require.Equal(t, int64(4), mm.Errors)
	require.Equal(t, int64(0), mm.LimitHits)
}

func TestHbStreamPartsPassThroughInOrder(t *testing.T) {
	hbIsolate(t)
	want := []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "a"},
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "hello"},
		{Type: fantasy.StreamPartTypeTextDelta, Delta: " world"},
		{Type: fantasy.StreamPartTypeFinish},
	}
	m := &hbFakeModel{provider: "prov", model: "hbm", parts: want}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-order"), "config-id")
	require.NoError(t, err)
	s, err := lm.Stream(hbAttributed("hb-order"), fantasy.Call{})
	require.NoError(t, err)
	var got []fantasy.StreamPart
	for part := range s {
		got = append(got, part)
	}
	require.Equal(t, want, got)
	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-order")
	require.Equal(t, int64(0), entry.Models["prov/hbm"].Errors)
}

func TestHbStreamErrorPartRecordsOneFailure(t *testing.T) {
	hbIsolate(t)
	boom := errors.New("hb-part-boom")
	m := &hbFakeModel{provider: "prov", model: "hbm", parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "x"},
		{Type: fantasy.StreamPartTypeError, Error: boom},
		{Type: fantasy.StreamPartTypeFinish},
	}}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-part-err"), "config-id")
	require.NoError(t, err)
	s, err := lm.Stream(hbAttributed("hb-part-err"), fantasy.Call{})
	require.NoError(t, err)
	var got []fantasy.StreamPart
	for part := range s {
		got = append(got, part)
	}
	require.Len(t, got, 3)
	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-part-err")
	mm := entry.Models["prov/hbm"]
	require.Equal(t, int64(1), mm.Requests)
	require.Equal(t, int64(1), mm.Errors, "exactly one failure per observed error part")
}

func TestHbStreamEarlyStopRecordsNoFailure(t *testing.T) {
	hbIsolate(t)
	m := &hbFakeModel{provider: "prov", model: "hbm", parts: []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "first"},
		{Type: fantasy.StreamPartTypeError, Error: errors.New("never observed")},
		{Type: fantasy.StreamPartTypeTextDelta, Delta: "last"},
	}}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-early"), "config-id")
	require.NoError(t, err)
	s, err := lm.Stream(hbAttributed("hb-early"), fantasy.Call{})
	require.NoError(t, err)
	for part := range s {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			break
		}
	}
	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-early")
	mm := entry.Models["prov/hbm"]
	require.Equal(t, int64(1), mm.Requests)
	require.Equal(t, int64(0), mm.Errors, "consumer stop-early must not record a failure")
}

func TestHbRecordsWrappedModelIdentity(t *testing.T) {
	hbIsolate(t)
	m := &hbFakeModel{provider: "real-prov", model: "real-model", resp: &fantasy.Response{}}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-ident"), "config-id")
	require.NoError(t, err)
	_, err = lm.Generate(hbAttributed("hb-ident"), fantasy.Call{})
	require.NoError(t, err)
	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-ident")
	require.NotNil(t, entry.Models["real-prov/real-model"], "must record the wrapped model's own identity")
	require.Nil(t, entry.Models["config-id"], "config id must not be recorded")
}

func TestHbRedactsBearerTokenInError(t *testing.T) {
	dir := hbIsolate(t)
	m := &hbFakeModel{
		provider: "prov", model: "hbm",
		genErr: errors.New("request failed: Authorization: Bearer supersecrettoken123456"),
	}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-secret"), "config-id")
	require.NoError(t, err)
	_, err = lm.Generate(hbAttributed("hb-secret"), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("TOPSECRET-PROMPT")}})
	require.Error(t, err)
	hbShutdownFlush(t)
	var published strings.Builder
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f.Name()))
		require.NoError(t, err)
		published.Write(b)
	}
	snapshot := published.String()
	require.NotContains(t, snapshot, "supersecrettoken123456")
	require.NotContains(t, snapshot, "TOPSECRET-PROMPT")
	require.Contains(t, snapshot, "Bearer [REDACTED]")
}

func TestHbContextWithoutAttributionUsesNone(t *testing.T) {
	hbIsolate(t)
	m := &hbFakeModel{provider: "prov", model: "hbm", resp: &fantasy.Response{}}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(context.Background(), "config-id")
	require.NoError(t, err)
	_, err = lm.Generate(context.Background(), fantasy.Call{})
	require.NoError(t, err)
	entry := hbFindEntry(t, hbShutdownFlush(t), "_none")
	require.NotNil(t, entry.Models["prov/hbm"])
}

func TestHbConcurrentRecording(t *testing.T) {
	hbIsolate(t)
	m := &hbFakeModel{provider: "prov", model: "hbm", resp: &fantasy.Response{}}
	p := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm, err := p.LanguageModel(hbAttributed("hb-conc"), "config-id")
	require.NoError(t, err)
	ctx := hbAttributed("hb-conc")
	const calls = 32
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := lm.Generate(ctx, fantasy.Call{})
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-conc")
	require.Equal(t, int64(calls), entry.Models["prov/hbm"].Requests)
}

func TestHbIdempotentWrap(t *testing.T) {
	hbIsolate(t)
	inner := &hbFakeProvider{m: &hbFakeModel{provider: "prov", model: "hbm"}}
	once := NewHeartbeatProvider(inner)
	twice := NewHeartbeatProvider(once)
	require.Equal(t, once, twice, "double wrap must return the same decorator")
	require.Nil(t, NewHeartbeatProvider(nil))
}

func TestHbProviderLanguageModelDelegates(t *testing.T) {
	hbIsolate(t)
	sentinel := errors.New("hb-lm-boom")
	p := NewHeartbeatProvider(&hbFakeProvider{m: nil, err: sentinel})
	lm, err := p.LanguageModel(context.Background(), "config-id")
	require.Nil(t, lm)
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, "hb-fake", p.Name())

	m := &hbFakeModel{provider: "real-prov", model: "real-model"}
	p2 := NewHeartbeatProvider(&hbFakeProvider{m: m})
	lm2, err := p2.LanguageModel(context.Background(), "config-id")
	require.NoError(t, err)
	require.Equal(t, "real-prov", lm2.Provider())
	require.Equal(t, "real-model", lm2.Model())
	// Catalog/listing calls must not count as requests. Shutdown republishes
	// historical rows, so assert only on this test's own unique session.
	ctxLm := hbAttributed("hb-lm-delegate")
	p2.LanguageModel(ctxLm, "config-id")
	p2.LanguageModel(ctxLm, "config-id")
	for _, e := range hbShutdownFlush(t) {
		require.NotEqual(t, "hb-lm-delegate", e.Session, "listing recorded a request")
	}
}
