package usage

import (
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
)

func usageIsZero(usage fantasy.Usage) bool {
	return usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.TotalTokens == 0 &&
		usage.ReasoningTokens == 0 &&
		usage.CacheCreationTokens == 0 &&
		usage.CacheReadTokens == 0
}

// approx is the root's approxTokenCount seam: injected through the entry
// points below and threaded into every helper that tokenizes text, evaluated
// at the same point the old direct call happened.
func fallbackStepUsage(messages []fantasy.Message, step fantasy.StepResult, approx func(string) int64) (fantasy.Usage, bool) {
	if !usageIsZero(step.Usage) {
		return step.Usage, false
	}

	inputTokens := estimateMessageTokens(messages, approx)
	outputTokens := estimateStepCompletionTokens(step, approx)
	if inputTokens == 0 && outputTokens == 0 {
		return fantasy.Usage{}, false
	}

	return fantasy.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}, true
}

// cloneFantasyMessages returns a snapshot of messages that subsequent
// mutation of the live conversation cannot reach: the slice itself, each
// message's Content slice, every part (value and pointer form), FilePart
// byte payloads, and every ProviderOptions map together with the option
// values inside them are copied. This is what the cache keep-alive replay
// (scheduleCacheKeepAlive) replays: its whole value is byte-matching the
// prompt prefix the triggering turn cached, so a later turn mutating shared
// internals retroactively changing the snapshot would silently invalidate
// the cache the replay exists to keep warm.
//
// Deliberately left SHARED, and why that is safe:
//   - string fields everywhere (part text, tool-call IDs and inputs, media
//     base64): Go strings are immutable; "changing" one in the original can
//     only replace the field on the original's own copy, which the snapshot
//     does not alias.
//   - ToolResultOutputContentError.Error (an error interface value): errors
//     are created, formatted and read, never mutated in place by this repo
//     or fantasy; copying an arbitrary error value is not possible
//     generically.
//   - MessagePart and ProviderOptionsData implementations unknown to the
//     switches below: passed through untouched (never dropped) — fantasy's
//     closed part set and every option type this repo attaches are covered,
//     and an unknown implementation could not be copied without reflection.
func cloneFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	cloned := make([]fantasy.Message, len(messages))
	for i, msg := range messages {
		cloned[i] = msg
		cloned[i].ProviderOptions = cloneProviderOptions(msg.ProviderOptions)
		cloned[i].Content = cloneMessageParts(msg.Content)
	}
	return cloned
}

// cloneMessageParts deep-copies a Content slice; nil and empty stay nil,
// matching the append-based clone this replaces.
func cloneMessageParts(parts []fantasy.MessagePart) []fantasy.MessagePart {
	if len(parts) == 0 {
		return nil
	}
	cloned := make([]fantasy.MessagePart, len(parts))
	for i, part := range parts {
		cloned[i] = cloneMessagePart(part)
	}
	return cloned
}

// cloneMessagePart deep-copies one of fantasy's five MessagePart types,
// preserving the value-vs-pointer dynamic type of the original. Pointer
// parts must get a fresh pointee: sharing one would alias every field, not
// just the mutable ones.
func cloneMessagePart(part fantasy.MessagePart) fantasy.MessagePart {
	switch p := part.(type) {
	case fantasy.TextPart:
		p.ProviderOptions = cloneProviderOptions(p.ProviderOptions)
		return p
	case *fantasy.TextPart:
		c := *p
		c.ProviderOptions = cloneProviderOptions(c.ProviderOptions)
		return &c
	case fantasy.ReasoningPart:
		p.ProviderOptions = cloneProviderOptions(p.ProviderOptions)
		return p
	case *fantasy.ReasoningPart:
		c := *p
		c.ProviderOptions = cloneProviderOptions(c.ProviderOptions)
		return &c
	case fantasy.FilePart:
		p.ProviderOptions = cloneProviderOptions(p.ProviderOptions)
		p.Data = append([]byte(nil), p.Data...)
		return p
	case *fantasy.FilePart:
		c := *p
		c.ProviderOptions = cloneProviderOptions(c.ProviderOptions)
		c.Data = append([]byte(nil), p.Data...)
		return &c
	case fantasy.ToolCallPart:
		p.ProviderOptions = cloneProviderOptions(p.ProviderOptions)
		return p
	case *fantasy.ToolCallPart:
		c := *p
		c.ProviderOptions = cloneProviderOptions(c.ProviderOptions)
		return &c
	case fantasy.ToolResultPart:
		p.ProviderOptions = cloneProviderOptions(p.ProviderOptions)
		return p
	case *fantasy.ToolResultPart:
		c := *p
		c.ProviderOptions = cloneProviderOptions(c.ProviderOptions)
		return &c
	default:
		return part
	}
}

// cloneProviderOptions deep-copies a ProviderOptions map: the map itself
// (so key inserts/deletes on the original cannot reach the snapshot) and
// every value inside it (the implementations this repo attaches are
// pointers, so sharing a value would share mutable state). nil stays nil.
func cloneProviderOptions(opts fantasy.ProviderOptions) fantasy.ProviderOptions {
	if opts == nil {
		return nil
	}
	cloned := make(fantasy.ProviderOptions, len(opts))
	for k, v := range opts {
		cloned[k] = cloneProviderOptionsData(v)
	}
	return cloned
}

// cloneProviderOptionsData copies the ProviderOptionsData implementations
// this repo can attach to a message (see getCacheControlOptions and
// message.ToAIMessage); each is a pointer, so a shallow map copy would
// still alias its fields.
func cloneProviderOptionsData(v fantasy.ProviderOptionsData) fantasy.ProviderOptionsData {
	switch o := v.(type) {
	case *anthropic.ProviderCacheControlOptions:
		c := *o
		return &c
	case *anthropic.ReasoningOptionMetadata:
		c := *o
		return &c
	case *google.ReasoningMetadata:
		c := *o
		return &c
	case *openai.ResponsesReasoningMetadata:
		c := *o
		if o.EncryptedContent != nil {
			s := *o.EncryptedContent
			c.EncryptedContent = &s
		}
		c.Summary = append([]string(nil), o.Summary...)
		return &c
	default:
		return v
	}
}

func estimateMessageTokens(messages []fantasy.Message, approx func(string) int64) int64 {
	var tokens int64
	for _, msg := range messages {
		tokens += approx(string(msg.Role))
		for _, part := range msg.Content {
			tokens += estimateMessagePartTokens(part, approx)
		}
	}
	return tokens
}

func estimateStepCompletionTokens(step fantasy.StepResult, approx func(string) int64) int64 {
	var tokens int64
	for _, content := range step.Content {
		switch c := content.(type) {
		case fantasy.TextContent:
			tokens += approx(c.Text)
		case *fantasy.TextContent:
			tokens += approx(c.Text)
		case fantasy.ReasoningContent:
			tokens += approx(c.Text)
		case *fantasy.ReasoningContent:
			tokens += approx(c.Text)
		case fantasy.FileContent:
			tokens += estimateGeneratedFileTokens(c, approx)
		case *fantasy.FileContent:
			tokens += estimateGeneratedFileTokens(*c, approx)
		case fantasy.SourceContent:
			tokens += estimateSourceTokens(c, approx)
		case *fantasy.SourceContent:
			tokens += estimateSourceTokens(*c, approx)
		case fantasy.ToolCallContent:
			tokens += estimateToolCallTokens(c.ToolName, c.Input, approx)
		case *fantasy.ToolCallContent:
			tokens += estimateToolCallTokens(c.ToolName, c.Input, approx)
		case fantasy.ToolResultContent:
			if c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.ClientMetadata, c.Result, approx)
			}
		case *fantasy.ToolResultContent:
			if c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.ClientMetadata, c.Result, approx)
			}
		}
	}
	return tokens
}

func estimateMessagePartTokens(part fantasy.MessagePart, approx func(string) int64) int64 {
	switch p := part.(type) {
	case fantasy.TextPart:
		return approx(p.Text)
	case *fantasy.TextPart:
		return approx(p.Text)
	case fantasy.ReasoningPart:
		return approx(p.Text)
	case *fantasy.ReasoningPart:
		return approx(p.Text)
	case fantasy.FilePart:
		return estimateFilePartTokens(p, approx)
	case *fantasy.FilePart:
		return estimateFilePartTokens(*p, approx)
	case fantasy.ToolCallPart:
		return estimateToolCallTokens(p.ToolName, p.Input, approx)
	case *fantasy.ToolCallPart:
		return estimateToolCallTokens(p.ToolName, p.Input, approx)
	case fantasy.ToolResultPart:
		return estimateToolResultContentTokens(p.ToolCallID, "", "", p.Output, approx)
	case *fantasy.ToolResultPart:
		return estimateToolResultContentTokens(p.ToolCallID, "", "", p.Output, approx)
	default:
		return 0
	}
}

func estimateToolCallTokens(toolName, input string, approx func(string) int64) int64 {
	return approx(toolName) + approx(input)
}

func estimateToolResultContentTokens(toolCallID, toolName, metadata string, output fantasy.ToolResultOutputContent, approx func(string) int64) int64 {
	tokens := approx(toolCallID) + approx(toolName) + approx(metadata)
	switch result := output.(type) {
	case fantasy.ToolResultOutputContentText:
		tokens += approx(result.Text)
	case *fantasy.ToolResultOutputContentText:
		tokens += approx(result.Text)
	case fantasy.ToolResultOutputContentError:
		if result.Error != nil {
			tokens += approx(result.Error.Error())
		}
	case *fantasy.ToolResultOutputContentError:
		if result.Error != nil {
			tokens += approx(result.Error.Error())
		}
	case fantasy.ToolResultOutputContentMedia:
		tokens += estimateMediaTokens(result.MediaType, result.Text, len(result.Data), approx)
	case *fantasy.ToolResultOutputContentMedia:
		tokens += estimateMediaTokens(result.MediaType, result.Text, len(result.Data), approx)
	}
	return tokens
}

func estimateFilePartTokens(file fantasy.FilePart, approx func(string) int64) int64 {
	return estimateMediaTokens(file.MediaType, file.Filename, len(file.Data), approx)
}

func estimateGeneratedFileTokens(file fantasy.FileContent, approx func(string) int64) int64 {
	return estimateMediaTokens(file.MediaType, "", len(file.Data), approx)
}

func estimateMediaTokens(mediaType, text string, dataBytes int, approx func(string) int64) int64 {
	if dataBytes == 0 {
		return approx(mediaType) + approx(text)
	}
	return approx(fmt.Sprintf("%s %s %d bytes", mediaType, text, dataBytes))
}

func estimateSourceTokens(source fantasy.SourceContent, approx func(string) int64) int64 {
	return approx(string(source.SourceType)) +
		approx(source.ID) +
		approx(source.URL) +
		approx(source.Title) +
		approx(source.MediaType) +
		approx(source.Filename)
}

// UsageIsZero is the exported seam over usageIsZero.
func UsageIsZero(u fantasy.Usage) bool { return usageIsZero(u) }

// FallbackStepUsage is the exported seam over fallbackStepUsage; approx is
// the injected tokenizer (root's approxTokenCount).
func FallbackStepUsage(messages []fantasy.Message, step fantasy.StepResult, approx func(string) int64) (fantasy.Usage, bool) {
	return fallbackStepUsage(messages, step, approx)
}

// CloneFantasyMessages is the exported seam over cloneFantasyMessages.
func CloneFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	return cloneFantasyMessages(messages)
}
