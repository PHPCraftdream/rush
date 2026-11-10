package tools

import (
	"errors"

	"charm.land/fantasy"
)

var errEditOldStringNotFound = errors.New("old_string not found in file. Make sure it matches exactly, including whitespace and line breaks")

// EditEffectMetadata describes a native edit rejected before any file write.
type EditEffectMetadata struct {
	EditEffect struct {
		NoWrite bool   `json:"no_write"`
		Reason  string `json:"reason"`
	} `json:"edit_effect"`
}

func editNoWriteResponse(text, reason string) fantasy.ToolResponse {
	var metadata EditEffectMetadata
	metadata.EditEffect.NoWrite = true
	metadata.EditEffect.Reason = reason
	return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(text), metadata)
}

func editReplacementErrorResponse(err error, readRefreshed bool) fantasy.ToolResponse {
	if !readRefreshed && errors.Is(err, errEditOldStringNotFound) {
		return editNoWriteResponse(err.Error(), "missing_old")
	}
	return fantasy.NewTextErrorResponse(err.Error())
}
