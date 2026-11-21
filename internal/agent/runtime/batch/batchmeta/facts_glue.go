package batchmeta

import (
	"strconv"
	"strings"
)

// BatchNodeStateLive mirrors the store's live-state vocabulary.
func BatchNodeStateLive(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// ParseBatchQuestionMarker extracts (nodeID, token) from a child-question
// notice text whose header line is the production marker written by
// batchCoordinatorQuestionPublisher (batch_install_question.go):
// "[batch-question node=X token=N sequence=S depth=D child=C]". A text
// without a parseable marker (or token 0, which the publisher never writes)
// is not counted. The token parses straight into int64 -- the batch_nodes
// token column's type -- so no unbounded uint64->int64 conversion exists
// (CodeQL go/integer-conversion).
func ParseBatchQuestionMarker(text string) (string, int64, bool) {
	if !strings.HasPrefix(text, "[batch-question ") {
		return "", 0, false
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	var node string
	var token int64
	for _, field := range strings.Fields(header) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch name {
		case "node":
			node = value
		case "token":
			t, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", 0, false
			}
			token = t
		}
	}
	if node == "" || token == 0 {
		return "", 0, false
	}
	return node, token, true
}

// BatchQuestionMarkerChild extracts the child session field ("child=C") of a
// published question marker header; "" when absent. The companion of
// ParseBatchQuestionMarker, which deliberately keeps its session-package
// twin's signature.
func BatchQuestionMarkerChild(text string) string {
	if !strings.HasPrefix(text, "[batch-question ") {
		return ""
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	for _, field := range strings.Fields(header) {
		if name, value, ok := strings.Cut(field, "="); ok && name == "child" {
			return value
		}
	}
	return ""
}
