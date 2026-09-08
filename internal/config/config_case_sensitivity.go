package config

import "encoding/binary"

const (
	configVolumeCapabilityCaseSensitive = uint32(0x100)
	configVolumeCapabilityAttributeSize = 4 + 8*4
)

// configParseDarwinVolumeCapabilities parses length plus the first capability
// and valid arrays returned by getattrlist(2).
func configParseDarwinVolumeCapabilities(buffer []byte) (caseInsensitive, known bool) {
	if len(buffer) < configVolumeCapabilityAttributeSize ||
		binary.LittleEndian.Uint32(buffer[:4]) < configVolumeCapabilityAttributeSize {
		return false, false
	}
	capabilities := binary.LittleEndian.Uint32(buffer[4:8])
	valid := binary.LittleEndian.Uint32(buffer[20:24])
	if valid&configVolumeCapabilityCaseSensitive == 0 {
		return false, false
	}
	return capabilities&configVolumeCapabilityCaseSensitive == 0, true
}
