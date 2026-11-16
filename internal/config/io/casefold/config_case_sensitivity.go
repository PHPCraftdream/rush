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
	LinuxExt4SuperMagic       uint64 = 0xef53
	LinuxF2FSSuperMagic       uint64 = 0xf2f52010
	LinuxMSDOSSuperMagic      uint64 = 0x4d44
	LinuxExFATSuperMagic      uint64 = 0x2011bab0
	LinuxFSCasefoldFlag       uint32 = 0x40000000
	CaseDirectoryMaxEntries          = 64
	CaseDirectoryMaxNameBytes        = 4096
)

var errConfigCaseDirectoryBudgetExceeded = errors.New("config case probe directory budget exceeded")

// ErrCaseDirectoryBudgetExceeded returns the package's single sentinel identity.
func ErrCaseDirectoryBudgetExceeded() error { return errConfigCaseDirectoryBudgetExceeded }

func LinuxCaseSensitivityFromMetadata(filesystemType uint64, flags uint32, flagsKnown bool) ConfigCaseSensitivity {
	filesystemInsensitive := filesystemType == LinuxMSDOSSuperMagic ||
		filesystemType == LinuxExFATSuperMagic
	filesystemSensitive := flagsKnown && (filesystemType == LinuxExt4SuperMagic ||
		filesystemType == LinuxF2FSSuperMagic)
	directoryCasefold := flagsKnown && flags&LinuxFSCasefoldFlag != 0
	return ConfigCaseSensitivityFromMetadata(filesystemInsensitive, filesystemSensitive, directoryCasefold)
}

func FreeBSDCaseSensitivityFromFilesystemName(name string) ConfigCaseSensitivity {
	return ConfigCaseSensitivityFromMetadata(name == "msdosfs", false, false)
}

func CaseDirectoryBudgetExceeded(names []string) bool {
	if len(names) > CaseDirectoryMaxEntries {
		return true
	}
	totalNameBytes := 0
	for _, name := range names {
		if len(name) > CaseDirectoryMaxNameBytes-totalNameBytes {
			return true
		}
		totalNameBytes += len(name)
	}
	return false
}

type CaseDirectoryEntry struct {
	Name     string
	Identity fileid.FileIdentity
}

type CaseDirectorySnapshot struct {
	Identity fileid.FileIdentity
	ModTime  int64
	Size     int64
	Entries  []CaseDirectoryEntry
}

func ConfigCaseSensitivityFromDirectorySnapshots(
	dir string,
	readSnapshot func() (CaseDirectorySnapshot, error),
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
		configCaseDirectoryHasVariantNames(initial.Entries) {
		return ConfigCaseSensitivityUnknown
	}

	var candidate string
	variant := ""
	for _, entry := range initial.Entries {
		if variant = configASCIICaseVariant(entry.Name); variant != "" {
			candidate = entry.Name
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
		configCaseDirectoryHasVariantNames(final.Entries) {
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

func configCaseDirectorySnapshotsEqual(left, right CaseDirectorySnapshot) bool {
	return left.Identity.Valid && left.Identity == right.Identity &&
		left.ModTime == right.ModTime && left.Size == right.Size &&
		configCaseDirectoryEntriesEqual(left.Entries, right.Entries)
}

func configCaseDirectoryEntriesEqual(left, right []CaseDirectoryEntry) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name {
			return false
		}
	}
	return true
}

func configCaseDirectoryHasVariantNames(entries []CaseDirectoryEntry) bool {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		folded := configASCIINameFold(entry.Name)
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

func DarwinCaseSensitivityFromPathconf(value int, err error) ConfigCaseSensitivity {
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
