package promptcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"

	"charm.land/fantasy"
)

// CachePreparedDiagnostic describes logical request values, never wire bytes.
// Fingerprints are observations, not evidence that a provider reused a cache.
type CachePreparedDiagnostic struct {
	PrefixFingerprint string `json:"prefix_fingerprint"`
	ToolsFingerprint  string `json:"tools_fingerprint"`
	PrefixBytes       int64  `json:"prefix_logical_bytes"`
	ToolsBytes        int64  `json:"tools_logical_bytes"`
	MessageBytes      int64  `json:"message_logical_bytes"`
	ToolCount         int    `json:"tool_count"`
}

type cacheDigestWriter struct {
	h hash.Hash
	n int64
}

func (w *cacheDigestWriter) Write(p []byte) (int, error) {
	n, err := w.h.Write(p)
	w.n += int64(n)
	return n, err
}

func cacheLogicalDigest(value any) (string, int64) {
	w := &cacheDigestWriter{h: sha256.New()}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// An unencodable extension is unknown, not an empty request.
		return "", 0
	}
	return hex.EncodeToString(w.h.Sum(nil)), w.n
}

func PreparedCacheDiagnostic(messages []fantasy.Message, tools []fantasy.AgentTool) CachePreparedDiagnostic {
	prefixEnd := 0
	for prefixEnd < len(messages) && messages[prefixEnd].Role == fantasy.MessageRoleSystem {
		prefixEnd++
	}
	prefix, prefixBytes := cacheLogicalDigest(messages[:prefixEnd])
	definitions := make([]struct {
		Info    fantasy.ToolInfo
		Options fantasy.ProviderOptions
	}, len(tools))
	for i, tool := range tools {
		definitions[i].Info = tool.Info()
		definitions[i].Options = tool.ProviderOptions()
	}
	toolHash, toolBytes := cacheLogicalDigest(definitions)
	_, messageBytes := cacheLogicalDigest(messages)
	return CachePreparedDiagnostic{
		PrefixFingerprint: prefix, ToolsFingerprint: toolHash,
		PrefixBytes: prefixBytes, ToolsBytes: toolBytes,
		MessageBytes: messageBytes, ToolCount: len(tools),
	}
}
