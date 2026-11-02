package server

func cloneListenerPins(pins map[string]string) map[string]string {
	if pins == nil {
		return nil
	}
	copy := make(map[string]string, len(pins))
	for tokenID, sessionID := range pins {
		copy[tokenID] = sessionID
	}
	return copy
}
