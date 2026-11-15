package cmd

import tea "charm.land/bubbletea/v2"

func processStopKey(message tea.Msg) tea.Msg {
	if key, ok := message.(tea.KeyPressMsg); ok {
		code := key.BaseCode
		if code == 0 {
			code = key.Code
		}
		if key.Mod.Contains(tea.ModCtrl) && (code == 'c' || code == 'C') && requestProcessInterrupt() {
			return nil
		}
	}
	return message
}

func newCLIProgram(model tea.Model, options ...tea.ProgramOption) *tea.Program {
	if processStopInstalled.Load() {
		options = append(options, tea.WithoutSignalHandler(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			return processStopKey(msg)
		}))
	}
	return tea.NewProgram(model, options...)
}
