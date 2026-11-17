package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
	"github.com/stretchr/testify/require"
)

// hbIsolate points heartbeat at throwaway dirs for the duration of the test.
func hbIsolate(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", d)
	heartbeat.ResetForTest()
	t.Cleanup(heartbeat.ResetForTest)
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

