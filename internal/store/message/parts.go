package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy/providers/openai"
)

type Attachment struct {
	FilePath string
	FileName string
	MimeType string
	Content  []byte
}

func (a Attachment) IsText() bool     { return strings.HasPrefix(a.MimeType, "text/") }
func (a Attachment) IsImage() bool    { return strings.HasPrefix(a.MimeType, "image/") }
func (a Attachment) IsMarkdown() bool { return a.MimeType == "text/markdown" }

// ContainsTextAttachment returns true if any of the attachments is a text attachment.
func ContainsTextAttachment(attachments []Attachment) bool {
	return slices.ContainsFunc(attachments, func(a Attachment) bool {
		return a.IsText()
	})
}

// Serialization of a message's content parts. Split out of message.go when
// the 1000-line file limit landed; this is the only concern in the package
// that talks about the on-disk shape of a part rather than the service's
// behaviour.

type partType string

const (
	reasoningType  partType = "reasoning"
	textType       partType = "text"
	imageURLType   partType = "image_url"
	binaryType     partType = "binary"
	toolCallType   partType = "tool_call"
	toolResultType partType = "tool_result"
	finishType     partType = "finish"
)

type partWrapper struct {
	Type partType    `json:"type"`
	Data ContentPart `json:"data"`
}

func marshalParts(parts []ContentPart) ([]byte, error) {
	wrappedParts := make([]partWrapper, len(parts))

	for i, part := range parts {
		var typ partType

		switch part.(type) {
		case ReasoningContent:
			typ = reasoningType
		case TextContent:
			typ = textType
		case ImageURLContent:
			typ = imageURLType
		case BinaryContent:
			typ = binaryType
		case ToolCall:
			typ = toolCallType
		case ToolResult:
			typ = toolResultType
		case Finish:
			typ = finishType
		default:
			return nil, fmt.Errorf("unknown part type: %T", part)
		}

		wrappedParts[i] = partWrapper{
			Type: typ,
			Data: part,
		}
	}
	return json.Marshal(wrappedParts)
}

func unmarshalParts(data []byte) ([]ContentPart, error) {
	temp := []json.RawMessage{}

	if err := json.Unmarshal(data, &temp); err != nil {
		return nil, err
	}

	parts := make([]ContentPart, 0)

	for _, rawPart := range temp {
		var wrapper struct {
			Type partType        `json:"type"`
			Data json.RawMessage `json:"data"`
		}

		if err := json.Unmarshal(rawPart, &wrapper); err != nil {
			return nil, err
		}

		switch wrapper.Type {
		case reasoningType:
			var raw struct {
				ReasoningContent
				ResponsesData json.RawMessage `json:"responses_data"`
			}
			if err := json.Unmarshal(wrapper.Data, &raw); err != nil {
				return nil, err
			}
			responsesData, err := decodeResponsesReasoningMetadata(raw.ResponsesData)
			if err != nil {
				return nil, err
			}
			part := raw.ReasoningContent
			part.ResponsesData = responsesData
			parts = append(parts, part)
		case textType:
			part := TextContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case imageURLType:
			part := ImageURLContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case binaryType:
			part := BinaryContent{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolCallType:
			part := ToolCall{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case toolResultType:
			part := ToolResult{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case finishType:
			part := Finish{}
			if err := json.Unmarshal(wrapper.Data, &part); err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			return nil, fmt.Errorf("unknown part type: %s", wrapper.Type)
		}
	}

	return parts, nil
}

// decodeResponsesReasoningMetadata accepts the legacy flat representation and
// fantasy's provider-type wrapper without changing its normal JSON encoding.
func decodeResponsesReasoningMetadata(data json.RawMessage) (*openai.ResponsesReasoningMetadata, error) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, nil
	}
	var wrapped struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	if wrapped.Type != "" && len(wrapped.Data) != 0 {
		data = wrapped.Data
	}
	var metadata openai.ResponsesReasoningMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}
