package protocol

// MaxPayloadBytes bounds protocol JSON, including surrounding whitespace.
// Hosts must bound raw reads before allocating the RawMessage passed to Execute.
const MaxPayloadBytes = 4 * 1024 * 1024

// MaxPayloadDepth bounds simultaneous JSON object/array containers, not domain depth.
const MaxPayloadDepth = 128

// preflightPayload deliberately leaves syntax validation to the existing decoder.
// The scan needs no allocations and ignores containers inside JSON strings.
func preflightPayload(payload []byte) string {
	if len(payload) > MaxPayloadBytes {
		return "payload exceeds byte limit"
	}
	depth := 0
	quoted, escaped := false, false
	for _, b := range payload {
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > MaxPayloadDepth {
				return "payload exceeds structural depth limit"
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return ""
}
