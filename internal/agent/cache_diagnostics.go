package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
)

// cachePreparedDiagnostic describes logical request values, never wire bytes.
// Fingerprints are observations, not evidence that a provider reused a cache.
type cachePreparedDiagnostic struct {
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

func preparedCacheDiagnostic(messages []fantasy.Message, tools []fantasy.AgentTool) cachePreparedDiagnostic {
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
	return cachePreparedDiagnostic{
		PrefixFingerprint: prefix, ToolsFingerprint: toolHash,
		PrefixBytes: prefixBytes, ToolsBytes: toolBytes,
		MessageBytes: messageBytes, ToolCount: len(tools),
	}
}

// A plain fantasy.Usage loses field presence. Zero must remain unknown here
// until a provider supplies explicit presence evidence; reporter capability
// alone cannot prove that this particular response contained cached_tokens.
func observedCacheReadTokens(usage fantasy.Usage, estimated bool) *int64 {
	if estimated || usage.CacheReadTokens <= 0 {
		return nil
	}
	value := usage.CacheReadTokens
	return &value
}

func (ts *turnStream) logCacheStepUsage(usage fantasy.Usage, estimated bool) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	d := ts.cacheDiagnostic
	var cachedTokens any
	if observed := observedCacheReadTokens(usage, estimated); observed != nil {
		cachedTokens = *observed
	}
	slog.Debug("agent: prompt cache observation",
		"session_hash", session.HashID(ts.call.SessionID),
		"provider", ts.smartModel.ModelCfg.Provider,
		"model", ts.smartModel.ModelCfg.Model,
		"prefix_fingerprint", d.PrefixFingerprint,
		"tools_fingerprint", d.ToolsFingerprint,
		"prefix_logical_bytes", d.PrefixBytes,
		"tools_logical_bytes", d.ToolsBytes,
		"message_logical_bytes", d.MessageBytes,
		"tool_count", d.ToolCount,
		"prepare_ms", ts.cachePrepareDuration.Milliseconds(),
		"step_elapsed_ms", time.Since(ts.cachePreparedAt).Milliseconds(),
		"cached_tokens", cachedTokens,
		"estimated", estimated,
		"semantic_invalidation_expectation", "changed prefix or tool definitions may invalidate reuse; counters do not identify the cause",
		"byte_measurement", "logical JSON, not wire bytes or cache savings")
}
