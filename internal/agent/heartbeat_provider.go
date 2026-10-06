// Heartbeat decorator: records every fantasy model call in internal/heartbeat.

package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
)

// NewHeartbeatProvider wraps p so calls through any LanguageModel it hands
// out are recorded. Idempotent: an already-wrapped provider passes through.
func NewHeartbeatProvider(p fantasy.Provider) fantasy.Provider {
	if p == nil {
		return nil
	}
	if _, ok := p.(*hbProvider); ok {
		return p
	}
	return &hbProvider{inner: p}
}

type hbProvider struct{ inner fantasy.Provider }

func (h *hbProvider) Name() string { return h.inner.Name() }

func (h *hbProvider) LanguageModel(ctx context.Context, modelID string) (fantasy.LanguageModel, error) {
	m, err := h.inner.LanguageModel(ctx, modelID)
	if err != nil {
		return nil, err
	}
	return &hbLanguageModel{inner: m}, nil
}

// hbLanguageModel delegates every method verbatim and records one request at
// the start of each of the four call kinds. The identity recorded is what
// inner reports at call time, so mid-flight model swaps are attributed to
// the model actually used. Catalog/listing calls are not recorded.
type hbLanguageModel struct{ inner fantasy.LanguageModel }

func (m *hbLanguageModel) identity() (string, string) { return m.inner.Provider(), m.inner.Model() }

func (m *hbLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	p, id := m.identity()
	heartbeat.RecordRequest(ctx, p, id, nil)
	resp, err := m.inner.Generate(ctx, call)
	if err != nil {
		heartbeat.RecordFailure(ctx, p, id, err)
	}
	return resp, err
}

func (m *hbLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	p, id := m.identity()
	heartbeat.RecordRequest(ctx, p, id, nil)
	s, err := m.inner.Stream(ctx, call)
	if err != nil {
		heartbeat.RecordFailure(ctx, p, id, err)
		return nil, err
	}
	return observeStream(ctx, p, id, s), nil
}

func (m *hbLanguageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	p, id := m.identity()
	heartbeat.RecordRequest(ctx, p, id, nil)
	resp, err := m.inner.GenerateObject(ctx, call)
	if err != nil {
		heartbeat.RecordFailure(ctx, p, id, err)
	}
	return resp, err
}

func (m *hbLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	p, id := m.identity()
	heartbeat.RecordRequest(ctx, p, id, nil)
	s, err := m.inner.StreamObject(ctx, call)
	if err != nil {
		heartbeat.RecordFailure(ctx, p, id, err)
		return nil, err
	}
	return observeObjectStream(ctx, p, id, s), nil
}

func (m *hbLanguageModel) Provider() string { return m.inner.Provider() }

func (m *hbLanguageModel) Model() string { return m.inner.Model() }

// observeStream passes every part through unchanged and in order, recording
// one failure per observed error part. No goroutines; a consumer that stops
// early simply stops observing.
func observeStream(ctx context.Context, provider, model string, s fantasy.StreamResponse) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		s(func(part fantasy.StreamPart) bool {
			if part.Type == fantasy.StreamPartTypeError && part.Error != nil {
				heartbeat.RecordFailure(ctx, provider, model, part.Error)
			}
			return yield(part)
		})
	}
}

func observeObjectStream(ctx context.Context, provider, model string, s fantasy.ObjectStreamResponse) fantasy.ObjectStreamResponse {
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		s(func(part fantasy.ObjectStreamPart) bool {
			if part.Type == fantasy.ObjectStreamPartTypeError && part.Error != nil {
				heartbeat.RecordFailure(ctx, provider, model, part.Error)
			}
			return yield(part)
		})
	}
}
