package agent

// turnSmartServiceTier is the configured speed tier of the resolved smart model.
func turnSmartServiceTier(smartModel Model) string {
	return smartModel.ModelCfg.ServiceTier
}
