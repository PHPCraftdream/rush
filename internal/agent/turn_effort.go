package agent

// turnSmartReasoningEffort reports the effective effort pinned to this turn's
// resolved smart model. Session fields are deliberately not consulted: the
// resolved model configuration already incorporates applicable preferences.
func turnSmartReasoningEffort(smartModel Model) string {
	return smartModel.ModelCfg.ReasoningEffort
}

// turnSmartServiceTier is the configured speed tier of the resolved smart model.
func turnSmartServiceTier(smartModel Model) string {
	return smartModel.ModelCfg.ServiceTier
}
