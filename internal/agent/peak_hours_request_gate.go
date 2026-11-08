package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
)

type legacyPeakHoursBypassKey struct{}

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

func (c *coordinator) withPeakHoursRequestGate(provider fantasy.Provider, pc config.ProviderConfig) fantasy.Provider {
	return &peakHoursRequestProvider{Provider: provider, check: func(ctx context.Context) error {
		if opts := callOptionsFrom(ctx); opts != nil {
			if opts.AllowPeakHours {
				return nil
			}
		} else if allowed, _ := ctx.Value(legacyPeakHoursBypassKey{}).(bool); allowed {
			return nil
		}
		if pc.ID == "" {
			return checkPeakHours(pc)
		}
		return c.checkLivePeakHours(pc.ID)
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
