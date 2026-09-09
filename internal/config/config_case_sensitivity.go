package config

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type configCaseSensitivity uint8

const (
	configCaseSensitivityUnknown configCaseSensitivity = iota
	configCaseSensitivitySensitive
	configCaseSensitivityInsensitive
)

func configFoldCaseLeaf(leaf string, sensitivity configCaseSensitivity) (string, bool) {
	if sensitivity != configCaseSensitivityInsensitive || leaf == "" {
		return "", false
	}
	return strings.ToLower(leaf), true
}

func configCaseSensitivityFromMetadata(filesystemInsensitive, filesystemSensitive, directoryCasefold bool) configCaseSensitivity {
	if filesystemInsensitive || directoryCasefold {
		return configCaseSensitivityInsensitive
	}
	if filesystemSensitive {
		return configCaseSensitivitySensitive
	}
	return configCaseSensitivityUnknown
}

const (
	// Bound metadata-unknown directory probes to keep missing-path writes predictable.
	configLinuxExt4SuperMagic       uint64 = 0xef53
	configLinuxF2FSSuperMagic       uint64 = 0xf2f52010
	configLinuxMSDOSSuperMagic      uint64 = 0x4d44
	configLinuxExFATSuperMagic      uint64 = 0x2011bab0
	configLinuxFSCasefoldFlag       uint32 = 0x40000000
	configCaseDirectoryMaxEntries          = 64
	configCaseDirectoryMaxNameBytes        = 4096
)

var errConfigCaseDirectoryBudgetExceeded = errors.New("config case probe directory budget exceeded")

func configLinuxCaseSensitivityFromMetadata(filesystemType uint64, flags uint32, flagsKnown bool) configCaseSensitivity {
	filesystemInsensitive := filesystemType == configLinuxMSDOSSuperMagic ||
		filesystemType == configLinuxExFATSuperMagic
	filesystemSensitive := flagsKnown && (filesystemType == configLinuxExt4SuperMagic ||
		filesystemType == configLinuxF2FSSuperMagic)
	directoryCasefold := flagsKnown && flags&configLinuxFSCasefoldFlag != 0
	return configCaseSensitivityFromMetadata(filesystemInsensitive, filesystemSensitive, directoryCasefold)
}

func configFreeBSDCaseSensitivityFromFilesystemName(name string) configCaseSensitivity {
	return configCaseSensitivityFromMetadata(name == "msdosfs", false, false)
}

func configCaseDirectoryBudgetExceeded(names []string) bool {
	if len(names) > configCaseDirectoryMaxEntries {
		return true
	}
	totalNameBytes := 0
	for _, name := range names {
		if len(name) > configCaseDirectoryMaxNameBytes-totalNameBytes {
			return true
		}
		totalNameBytes += len(name)
	}
	return false
}

type configCaseDirectoryEntry struct {
	name     string
	identity configFileIdentity
}

type configCaseDirectorySnapshot struct {
	identity configFileIdentity
	modTime  int64
	size     int64
	entries  []configCaseDirectoryEntry
}

func configCaseSensitivityFromDirectorySnapshots(
	dir string,
	readSnapshot func() (configCaseDirectorySnapshot, error),
	probe func(string) (configFileIdentity, error),
) configCaseSensitivity {
	if readSnapshot == nil || probe == nil {
		return configCaseSensitivityUnknown
	}
	initial, err := readSnapshot()
	if err != nil {
		return configCaseSensitivityUnknown
	}
	middle, err := readSnapshot()
	if err != nil || !configCaseDirectorySnapshotsEqual(initial, middle) ||
		configCaseDirectoryHasVariantNames(initial.entries) {
		return configCaseSensitivityUnknown
	}

	var candidate string
	variant := ""
	for _, entry := range initial.entries {
		if variant = configASCIICaseVariant(entry.name); variant != "" {
			candidate = entry.name
			break
		}
	}
	if variant == "" {
		return configCaseSensitivityUnknown
	}
	originalIdentity, originalErr := probe(filepath.Join(dir, candidate))
	alternateIdentity, alternateErr := probe(filepath.Join(dir, variant))
	final, finalErr := readSnapshot()
	if finalErr != nil || !configCaseDirectorySnapshotsEqual(initial, final) ||
		configCaseDirectoryHasVariantNames(final.entries) {
		return configCaseSensitivityUnknown
	}
	if originalErr != nil || !originalIdentity.valid {
		return configCaseSensitivityUnknown
	}
	if os.IsNotExist(alternateErr) {
		return configCaseSensitivitySensitive
	}
	if alternateErr != nil || !alternateIdentity.valid || originalIdentity != alternateIdentity {
		return configCaseSensitivityUnknown
	}
	return configCaseSensitivityInsensitive
}

func configCaseDirectorySnapshotsEqual(left, right configCaseDirectorySnapshot) bool {
	return left.identity.valid && left.identity == right.identity &&
		left.modTime == right.modTime && left.size == right.size &&
		configCaseDirectoryEntriesEqual(left.entries, right.entries)
}

func configCaseDirectoryEntriesEqual(left, right []configCaseDirectoryEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].name != right[i].name {
			return false
		}
	}
	return true
}

func configCaseDirectoryHasVariantNames(entries []configCaseDirectoryEntry) bool {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		folded := configASCIINameFold(entry.name)
		if _, ok := seen[folded]; ok {
			return true
		}
		seen[folded] = struct{}{}
	}
	return false
}

func configASCIICaseVariant(name string) string {
	variant := []byte(name)
	for i, char := range variant {
		switch {
		case char >= 'a' && char <= 'z':
			variant[i] = char - ('a' - 'A')
			return string(variant)
		case char >= 'A' && char <= 'Z':
			variant[i] = char + ('a' - 'A')
			return string(variant)
		}
	}
	return ""
}

func configASCIINameFold(name string) string {
	folded := []byte(name)
	for i, char := range folded {
		if char >= 'A' && char <= 'Z' {
			folded[i] = char + ('a' - 'A')
		}
	}
	return string(folded)
}

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
