package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/term"
	"github.com/pkg/browser"
)

type oauthLinkModel struct {
	url    string
	status string
	width  int
	open   func(string) error
	copy   func(string) error
}

func (m *oauthLinkModel) Init() tea.Cmd { return nil }

func (m *oauthLinkModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := message.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		return m, nil
	}
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	code := key.BaseCode
	if code == 0 {
		code = key.Code
	}
	switch {
	case key.Code == tea.KeyF2 || code == 'o' || code == 'O':
		if err := m.open(m.url); err != nil {
			m.status = "Could not open browser: " + err.Error()
		} else {
			m.status = "Opened link in browser."
		}
	case key.Code == tea.KeyF3 || code == 'c' || code == 'C':
		if err := m.copy(m.url); err != nil {
			m.status = "Could not copy link: " + err.Error()
		} else {
			m.status = "Link copied to clipboard."
		}
	}
	return m, nil
}

func (m *oauthLinkModel) View() tea.View {
	var text strings.Builder
	text.WriteString("Open this URL to authenticate with your ChatGPT account:\n")
	width := m.width
	if width <= 1 {
		width = 80
	}
	width--
	for remaining := m.url; len(remaining) > 0; {
		part := min(len(remaining), width)
		text.WriteString(remaining[:part])
		text.WriteByte('\n')
		remaining = remaining[part:]
	}
	text.WriteString("\n[o/F2] Open link   [c/F3] Copy full link\n")
	if m.status != "" {
		text.WriteString(m.status)
		text.WriteByte('\n')
	}
	view := tea.NewView(text.String())
	view.KeyboardEnhancements.ReportAlternateKeys = true
	view.KeyboardEnhancements.ReportAllKeysAsEscapeCodes = true
	return view
}

func startOAuthLinkControls(ctx context.Context, url string) func() {
	model := &oauthLinkModel{url: url, open: browser.OpenURL, copy: clipboard.WriteAll}
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		fmt.Printf("Open this URL to authenticate with your ChatGPT account:\n%s\n\n[o/F2] Open link   [c/F3] Copy full link (interactive terminal only)\n", url)
		return func() {}
	}
	if stop, ok := startPhysicalOAuthLinkControls(ctx, model); ok {
		return stop
	}
	programCtx, cancel := context.WithCancel(ctx)
	program := tea.NewProgram(model, tea.WithContext(programCtx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := program.Run(); err != nil && programCtx.Err() == nil {
			fmt.Printf("Open this URL to authenticate with your ChatGPT account:\n%s\n\nInteractive link controls unavailable: %v\n", url, err)
		}
	}()
	return func() {
		program.Quit()
		cancel()
		<-done
	}
}
