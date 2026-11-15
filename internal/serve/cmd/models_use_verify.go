package cmd

import (
	"context"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
)

type modelVerification struct {
	label     string
	selection config.SelectedModel
	cfg       *config.Config
	resolver  config.VariableResolver
}

func verifyModelSelections(items []modelVerification, out func(string, ...any)) {
	for _, item := range items {
		pc, ok := item.cfg.Providers.Get(item.selection.Provider)
		if !ok {
			out("warning: %s model %s/%s is set but could not be verified (provider is not configured); not in the local catalog, cost/context-window unknown. Run `rush providers update %s` or retry online.\n", item.label, item.selection.Provider, item.selection.Model, item.selection.Provider)
			continue
		}
		models, reason, err := config.RefreshProviderModels(context.Background(), item.cfg, pc, item.selection.Provider, item.resolver, item.selection.Model)
		if err != nil {
			reason = "provider request failed"
		}
		if reason != "" {
			short := strings.TrimSpace(reason)
			out("warning: %s model %s/%s is set but could not be verified (%s); not in the local catalog, cost/context-window unknown. Run `rush providers update %s` or retry online.\n", item.label, item.selection.Provider, item.selection.Model, short, item.selection.Provider)
			continue
		}
		listed := false
		for _, model := range models {
			if model.ID == item.selection.Model {
				listed = true
				break
			}
		}
		if listed {
			out("note: %s model %s/%s is set; it was not in the local catalog but %s's model list confirms it exists (cached, no warning next time).\n", item.label, item.selection.Provider, item.selection.Model, item.selection.Provider)
		} else {
			out("warning: %s model %s/%s is set, but %s's model list does not contain it -- check for a typo. cost/context-window unknown.\n", item.label, item.selection.Provider, item.selection.Model, item.selection.Provider)
		}
	}
}
