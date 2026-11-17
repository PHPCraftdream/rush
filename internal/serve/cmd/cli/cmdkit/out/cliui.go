package cliout

import (
	tea "charm.land/bubbletea/v2"

	cliproc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
)

func processStopKey(message tea.Msg) tea.Msg {
	if key, ok := message.(tea.KeyPressMsg); ok {
		code := key.BaseCode
		if code == 0 {
			code = key.Code
		}
		if key.Mod.Contains(tea.ModCtrl) && (code == 'c' || code == 'C') && cliproc.RequestProcessInterrupt() {
			return nil
		}
	}
	return message
}

func newCLIProgram(model tea.Model, options ...tea.ProgramOption) *tea.Program {
	if cliproc.ProcessStopInstalled() {
		options = append(options, tea.WithoutSignalHandler(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			return processStopKey(msg)
		}))
	}
	return tea.NewProgram(model, options...)
}

// NewCLIProgram builds the CLI tea program with the process-stop key filter
// (exported; lane K1: cmd call sites keep the lowercase bridge name).
func NewCLIProgram(model tea.Model, options ...tea.ProgramOption) *tea.Program {
	return newCLIProgram(model, options...)
}
