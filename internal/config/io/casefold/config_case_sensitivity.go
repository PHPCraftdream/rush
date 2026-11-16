package casefold

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
)

type ConfigCaseSensitivity uint8

const (
	ConfigCaseSensitivityUnknown ConfigCaseSensitivity = iota
	ConfigCaseSensitivitySensitive
	ConfigCaseSensitivityInsensitive
)

func ConfigFoldCaseLeaf(leaf string, sensitivity ConfigCaseSensitivity) (string, bool) {
	if sensitivity != ConfigCaseSensitivityInsensitive || leaf == "" {
		return "", false
	}
	return strings.ToLower(leaf), true
}

func ConfigCaseSensitivityFromMetadata(filesystemInsensitive, filesystemSensitive, directoryCasefold bool) ConfigCaseSensitivity {
	if filesystemInsensitive || directoryCasefold {
		return ConfigCaseSensitivityInsensitive
	}
	if filesystemSensitive {
		return ConfigCaseSensitivitySensitive
	}
	return ConfigCaseSensitivityUnknown
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

func configLinuxCaseSensitivityFromMetadata(filesystemType uint64, flags uint32, flagsKnown bool) ConfigCaseSensitivity {
	filesystemInsensitive := filesystemType == configLinuxMSDOSSuperMagic ||
		filesystemType == configLinuxExFATSuperMagic
	filesystemSensitive := flagsKnown && (filesystemType == configLinuxExt4SuperMagic ||
		filesystemType == configLinuxF2FSSuperMagic)
	directoryCasefold := flagsKnown && flags&configLinuxFSCasefoldFlag != 0
	return ConfigCaseSensitivityFromMetadata(filesystemInsensitive, filesystemSensitive, directoryCasefold)
}

func configFreeBSDCaseSensitivityFromFilesystemName(name string) ConfigCaseSensitivity {
	return ConfigCaseSensitivityFromMetadata(name == "msdosfs", false, false)
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
	identity fileid.FileIdentity
}

type configCaseDirectorySnapshot struct {
	identity fileid.FileIdentity
	modTime  int64
	size     int64
	entries  []configCaseDirectoryEntry
}

func ConfigCaseSensitivityFromDirectorySnapshots(
	dir string,
	readSnapshot func() (configCaseDirectorySnapshot, error),
	probe func(string) (fileid.FileIdentity, error),
) ConfigCaseSensitivity {
	if readSnapshot == nil || probe == nil {
		return ConfigCaseSensitivityUnknown
	}
	initial, err := readSnapshot()
	if err != nil {
		return ConfigCaseSensitivityUnknown
	}
	middle, err := readSnapshot()
	if err != nil || !configCaseDirectorySnapshotsEqual(initial, middle) ||
		configCaseDirectoryHasVariantNames(initial.entries) {
		return ConfigCaseSensitivityUnknown
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
		return ConfigCaseSensitivityUnknown
	}
	originalIdentity, originalErr := probe(filepath.Join(dir, candidate))
	alternateIdentity, alternateErr := probe(filepath.Join(dir, variant))
	final, finalErr := readSnapshot()
	if finalErr != nil || !configCaseDirectorySnapshotsEqual(initial, final) ||
		configCaseDirectoryHasVariantNames(final.entries) {
		return ConfigCaseSensitivityUnknown
	}
	if originalErr != nil || !originalIdentity.Valid {
		return ConfigCaseSensitivityUnknown
	}
	if os.IsNotExist(alternateErr) {
		return ConfigCaseSensitivitySensitive
	}
	if alternateErr != nil || !alternateIdentity.Valid || originalIdentity != alternateIdentity {
		return ConfigCaseSensitivityUnknown
	}
	return ConfigCaseSensitivityInsensitive
}

func configCaseDirectorySnapshotsEqual(left, right configCaseDirectorySnapshot) bool {
	return left.identity.Valid && left.identity == right.identity &&
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

func configDarwinCaseSensitivityFromPathconf(value int, err error) ConfigCaseSensitivity {
	if err != nil {
		return ConfigCaseSensitivityUnknown
	}
	switch value {
	case 0:
		return ConfigCaseSensitivityInsensitive
	case 1:
		return ConfigCaseSensitivitySensitive
	default:
		return ConfigCaseSensitivityUnknown
	}
}
