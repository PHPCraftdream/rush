// Diagnostic logging helpers: argument redaction, byte-length
// accounting and head/tail clipping for Stream's launch and exit logs.

package cliprovider

import (
	"fmt"
	"os"
)

// argsByteLen totals the bytes of all args — useful when diagnosing
// command-line-length issues on Windows (CreateProcessW has a 32K limit).
func argsByteLen(args []string) int {
	n := 0
	for _, a := range args {
		n += len(a) + 1 // +1 for the separator
	}
	return n
}

// clipString returns the first n chars of s with a "(+K more)" suffix when
// truncated. Used in slog fields to keep a sample of long prompts without
// blowing up the log file.
func clipString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("…(+%d more)", len(s)-n)
}

// tailString returns the last n chars of s with a "(+K skipped)" prefix when
// truncated. Pair with clipString to see prompt boundaries.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("(+%d skipped)…", len(s)-n) + s[len(s)-n:]
}

// logRawPromptEnabled returns true when raw prompt logging is explicitly enabled
// via the CRUSH_CLIPROVIDER_LOG_RAW_PROMPT environment variable. This is an
// opt-in diagnostic mode for debugging CLI invocation issues — it defaults to
// false to avoid leaking sensitive data (system prompts, API keys, tokens) into logs.
//
// Exported for use by agent.go's orphan outbox logging (SEC-1 fix).
func LogRawPromptEnabled() bool {
	return os.Getenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT") == "1"
}

// logRawPromptEnabled is a convenience alias for the exported function.
func logRawPromptEnabled() bool {
	return LogRawPromptEnabled()
}

// sanitizeArgs returns a safe-to-log version of args by redacting values of
// sensitive flags (like -p/--prompt) while preserving flag names and safe values
// (like config file paths). In normal mode, only flag names are logged for sensitive
// args. In diagnostic mode (CRUSH_CLIPROVIDER_LOG_RAW_PROMPT=1), the original args
// are returned as-is for debugging.
func sanitizeArgs(args []string) []string {
	if logRawPromptEnabled() {
		// Diagnostic mode: return original args
		return args
	}

	var result []string
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Known sensitive flags that may contain user data or secrets
		isSensitiveFlag := false
		switch arg {
		case "-p", "--prompt":
			isSensitiveFlag = true
		}

		if isSensitiveFlag && i+1 < len(args) {
			// Redact the value: keep flag name, replace value with placeholder
			result = append(result, arg, "[REDACTED]")
			i++ // Skip the next arg (the value)
		} else {
			// Keep safe flags and their values (e.g., --model sonnet, --mcp-config /path/to/file)
			result = append(result, arg)
		}
	}
	return result
}
