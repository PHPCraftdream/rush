package shell

import (
	"encoding/base64"
	"unicode/utf16"
	"unicode/utf8"
)

// DecodePowerShellEncodedCommand decodes the documented UTF-16LE payload.
// This strict form preserves agentguard's established behavior.
func DecodePowerShellEncodedCommand(encoded string) (string, bool) {
	return decodePowerShellEncodedCommand(encoded, false)
}

// DecodePowerShellEncodedCommandWithUTF8 also accepts UTF-8 payloads used by
// some compatible runtimes. The boolean is false for malformed data.
func DecodePowerShellEncodedCommandWithUTF8(encoded string) (string, bool) {
	return decodePowerShellEncodedCommand(encoded, true)
}

func decodePowerShellEncodedCommand(encoded string, allowUTF8 bool) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}

	if len(raw)%2 == 0 && (!allowUTF8 || hasUTF16LEASCIIShape(raw)) {
		u16 := make([]uint16, len(raw)/2)
		for i := range u16 {
			u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
		}
		return string(utf16.Decode(u16)), true
	}
	if allowUTF8 && utf8.Valid(raw) {
		return string(raw), true
	}
	return "", false
}

func hasUTF16LEASCIIShape(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	zeroes := 0
	for i := 1; i < len(raw); i += 2 {
		if raw[i] == 0 {
			zeroes++
		}
	}
	return zeroes > 0
}
