package agent

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

func fallbackStepUsage(messages []fantasy.Message, step fantasy.StepResult) (fantasy.Usage, bool) {
	if !usageIsZero(step.Usage) {
		return step.Usage, false
	}

	inputTokens := estimateMessageTokens(messages)
	outputTokens := estimateStepCompletionTokens(step)
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

func estimateMessageTokens(messages []fantasy.Message) int64 {
	var tokens int64
	for _, msg := range messages {
		tokens += approxTokenCount(string(msg.Role))
		for _, part := range msg.Content {
			tokens += estimateMessagePartTokens(part)
		}
	}
	return tokens
}

func estimateStepCompletionTokens(step fantasy.StepResult) int64 {
	var tokens int64
	for _, content := range step.Content {
		switch c := content.(type) {
		case fantasy.TextContent:
			tokens += approxTokenCount(c.Text)
		case *fantasy.TextContent:
			tokens += approxTokenCount(c.Text)
		case fantasy.ReasoningContent:
			tokens += approxTokenCount(c.Text)
		case *fantasy.ReasoningContent:
			tokens += approxTokenCount(c.Text)
		case fantasy.FileContent:
			tokens += estimateGeneratedFileTokens(c)
		case *fantasy.FileContent:
			tokens += estimateGeneratedFileTokens(*c)
		case fantasy.SourceContent:
			tokens += estimateSourceTokens(c)
		case *fantasy.SourceContent:
			tokens += estimateSourceTokens(*c)
		case fantasy.ToolCallContent:
			tokens += estimateToolCallTokens(c.ToolName, c.Input)
		case *fantasy.ToolCallContent:
			tokens += estimateToolCallTokens(c.ToolName, c.Input)
		case fantasy.ToolResultContent:
			if c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.ClientMetadata, c.Result)
			}
		case *fantasy.ToolResultContent:
			if c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.ClientMetadata, c.Result)
			}
		}
	}
	return tokens
}

func estimateMessagePartTokens(part fantasy.MessagePart) int64 {
	switch p := part.(type) {
	case fantasy.TextPart:
		return approxTokenCount(p.Text)
	case *fantasy.TextPart:
		return approxTokenCount(p.Text)
	case fantasy.ReasoningPart:
		return approxTokenCount(p.Text)
	case *fantasy.ReasoningPart:
		return approxTokenCount(p.Text)
	case fantasy.FilePart:
		return estimateFilePartTokens(p)
	case *fantasy.FilePart:
		return estimateFilePartTokens(*p)
	case fantasy.ToolCallPart:
		return estimateToolCallTokens(p.ToolName, p.Input)
	case *fantasy.ToolCallPart:
		return estimateToolCallTokens(p.ToolName, p.Input)
	case fantasy.ToolResultPart:
		return estimateToolResultContentTokens(p.ToolCallID, "", "", p.Output)
	case *fantasy.ToolResultPart:
		return estimateToolResultContentTokens(p.ToolCallID, "", "", p.Output)
	default:
		return 0
	}
}

func estimateToolCallTokens(toolName, input string) int64 {
	return approxTokenCount(toolName) + approxTokenCount(input)
}

func estimateToolResultContentTokens(toolCallID, toolName, metadata string, output fantasy.ToolResultOutputContent) int64 {
	tokens := approxTokenCount(toolCallID) + approxTokenCount(toolName) + approxTokenCount(metadata)
	switch result := output.(type) {
	case fantasy.ToolResultOutputContentText:
		tokens += approxTokenCount(result.Text)
	case *fantasy.ToolResultOutputContentText:
		tokens += approxTokenCount(result.Text)
	case fantasy.ToolResultOutputContentError:
		if result.Error != nil {
			tokens += approxTokenCount(result.Error.Error())
		}
	case *fantasy.ToolResultOutputContentError:
		if result.Error != nil {
			tokens += approxTokenCount(result.Error.Error())
		}
	case fantasy.ToolResultOutputContentMedia:
		tokens += estimateMediaTokens(result.MediaType, result.Text, len(result.Data))
	case *fantasy.ToolResultOutputContentMedia:
		tokens += estimateMediaTokens(result.MediaType, result.Text, len(result.Data))
	}
	return tokens
}

func estimateFilePartTokens(file fantasy.FilePart) int64 {
	return estimateMediaTokens(file.MediaType, file.Filename, len(file.Data))
}

func estimateGeneratedFileTokens(file fantasy.FileContent) int64 {
	return estimateMediaTokens(file.MediaType, "", len(file.Data))
}

func estimateMediaTokens(mediaType, text string, dataBytes int) int64 {
	if dataBytes == 0 {
		return approxTokenCount(mediaType) + approxTokenCount(text)
	}
	return approxTokenCount(fmt.Sprintf("%s %s %d bytes", mediaType, text, dataBytes))
}

func estimateSourceTokens(source fantasy.SourceContent) int64 {
	return approxTokenCount(string(source.SourceType)) +
		approxTokenCount(source.ID) +
		approxTokenCount(source.URL) +
		approxTokenCount(source.Title) +
		approxTokenCount(source.MediaType) +
		approxTokenCount(source.Filename)
}

func approxTokenCount(s string) int64 {
	if s == "" {
		return 0
	}
	return int64((len(s) + 3) / 4)
}
