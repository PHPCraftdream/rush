package agenticfetch

import (
	_ "embed"
)

//go:embed templates/agentic_fetch.md
var toolDescription string

//go:embed templates/agentic_fetch_prompt.md.tpl
var promptTmpl []byte

func ToolDescription() string {
	return toolDescription
}

func PromptTemplate() []byte {
	return promptTmpl
}
