// Config input fingerprints: file snapshots and discovery/alias-chain digests.
package docread

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
)

// SameBytesFingerprint reports whether data matches the supplied content digest.
func SameBytesFingerprint(data []byte, digest [sha256.Size]byte) bool {
	return sha256.Sum256(data) == digest
}

// FileSnapshot captures metadata about a config file at a point in time.
type FileSnapshot struct {
	Path        string
	Exists      bool
	Size        int64
	ModTime     int64 // UnixNano.
	ContentHash [32]byte
	fingerprint Fingerprint
}

// Fingerprint is the opaque snapshot token for one config document.
type Fingerprint struct {
	Exists          bool
	Size            int64
	ModTime         int64
	Digest          [sha256.Size]byte
	Owner           int
	Nlink           uint64
	Identity        fileid.FileIdentity
	Discovery       [sha256.Size]byte
	ParentDiscovery [sha256.Size]byte
	ParentIdentity  fileid.FileIdentity
	// AliasChain excludes mutable regular-file metadata at the leaf.
	AliasChain [sha256.Size]byte
}

// Fingerprint returns the snapshot's opaque file fingerprint.
func (snapshot FileSnapshot) Fingerprint() Fingerprint { return snapshot.fingerprint }

// NewFileSnapshot constructs a file snapshot while keeping its opaque fingerprint private.
func NewFileSnapshot(path string, fingerprint Fingerprint) FileSnapshot {
	return FileSnapshot{
		Path: path, Exists: fingerprint.Exists, Size: fingerprint.Size,
		ModTime: fingerprint.ModTime, ContentHash: fingerprint.Digest,
		fingerprint: fingerprint,
	}
}

// ConfigAliasChainFingerprint records the lexical path and every directory or
// symlink in its discovery chain. Metadata belonging only to a regular-file
// leaf is omitted so a cooperating atomic writer may replace its inode while
// retaining the same physical binding.
func ConfigAliasChainFingerprint(path string) [sha256.Size]byte {
	h := sha256.New()
	abs := configFingerprintPath(path)
	var components []string
	for current := abs; ; current = filepath.Dir(current) {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	slices.Reverse(components)
	for i, component := range components {
		_, _ = io.WriteString(h, component)
		_, _ = io.WriteString(h, "\x00")
		info, statErr := os.Lstat(component)
		if statErr != nil {
			_, _ = io.WriteString(h, "missing:")
			_, _ = io.WriteString(h, statErr.Error())
			_, _ = io.WriteString(h, "\x00")
			continue
		}
		isLeaf := i == len(components)-1
		if isLeaf && info.Mode().IsRegular() {
			// Regular-file permissions and inode identity belong to the fresh
			// descriptor fingerprint, not to the stable alias chain. Atomic
			// publication is allowed to change both while retaining the same
			// logical target.
			_, _ = io.WriteString(h, "regular-leaf|regular")
		} else {
			writeConfigDiscoveryInfo(h, info)
			if info.Mode()&os.ModeSymlink != 0 {
				if target, readErr := os.Readlink(component); readErr == nil {
					_, _ = io.WriteString(h, target)
				}
			}
		}
		_, _ = io.WriteString(h, "\x00")
	}
	return sha256.Sum256(h.Sum(nil))
}

// ConfigDiscoveryFingerprint includes every lexical component leading to the
// file. It makes a symlink retarget or a parent-directory replacement dirty
// even when the final target and bytes happen to return to their old values.
func ConfigDiscoveryFingerprint(path string) [sha256.Size]byte {
	h := sha256.New()
	abs := configFingerprintPath(path)
	var components []string
	for current := abs; ; current = filepath.Dir(current) {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	slices.Reverse(components)
	for _, component := range components {
		_, _ = io.WriteString(h, component)
		_, _ = io.WriteString(h, "\x00")
		info, statErr := os.Lstat(component)
		if statErr != nil {
			_, _ = io.WriteString(h, "missing:")
			if os.IsNotExist(statErr) {
				_, _ = io.WriteString(h, "not-exist")
			} else {
				_, _ = io.WriteString(h, statErr.Error())
			}
			_, _ = io.WriteString(h, "\x00")
			continue
		}
		writeConfigDiscoveryInfo(h, info)
		if info.Mode()&os.ModeSymlink != 0 {
			if target, readErr := os.Readlink(component); readErr == nil {
				_, _ = io.WriteString(h, target)
			}
		}
		_, _ = io.WriteString(h, "\x00")
	}
	return sha256.Sum256(h.Sum(nil))
}

func ReloadStalenessState(paths []string, fingerprints map[string]Fingerprint) ([]string, map[string]FileSnapshot) {
	paths = ExpandConfigSlotSiblings(paths)
	trackedSet := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			trackedSet[NormalizeDiscoveryPath(path)] = struct{}{}
		}
	}
	tracked := make([]string, 0, len(trackedSet))
	for path := range trackedSet {
		tracked = append(tracked, path)
	}
	slices.Sort(tracked)

	snapshots := make(map[string]FileSnapshot, len(tracked))
	for _, path := range tracked {
		// The fingerprints map only holds SELECTED documents; an unselected
		// slot sibling that exists on disk (rush.ktav selected, ignored
		// rush.json present) must still contribute its real on-disk
		// fingerprint, or the snapshot would publish Exists:false for an
		// existing file and ConfigStaleness would report Dirty forever.
		fingerprint, fpErr := TrackedSlotFingerprint(path, fingerprints)
		if fpErr != nil {
			fingerprint = Fingerprint{}
		}
		snapshots[path] = FileSnapshot{
			Path:        path,
			Exists:      fingerprint.Exists,
			Size:        fingerprint.Size,
			ModTime:     fingerprint.ModTime,
			ContentHash: fingerprint.Digest,
			fingerprint: fingerprint,
		}
	}
	return tracked, snapshots
}

const stableReadMaxAttempts = 4

// configFingerprintPath folds the leaf spelling when the containing directory
// is case-insensitive. Case-only aliases are one filesystem object there and
// must not produce different staleness fingerprints.
func configFingerprintPath(path string) string {
	abs := NormalizeDiscoveryPath(path)
	dir, leaf := filepath.Dir(abs), filepath.Base(abs)
	if folded, ok := casefold.ConfigPlatformCaseFoldLeaf(dir, leaf); ok {
		return filepath.Join(dir, folded)
	}
	return abs
}

func writeConfigDiscoveryInfo(h hash.Hash, info os.FileInfo) {
	_, _ = io.WriteString(h, info.Mode().String())
	// Directory mtimes change for unrelated files created beside a candidate;
	// their physical identity is the part relevant to parent replacement.
	// Regular-file metadata remains part of the exact file fingerprint below.
	if info.IsDir() {
		_, _ = io.WriteString(h, fmt.Sprintf("|dir|%d|", info.Mode()))
	} else {
		_, _ = io.WriteString(h, fmt.Sprintf("|%d|%d|%d|", info.Size(), info.ModTime().UnixNano(), info.Mode()))
	}
	identity := fileid.FileIdentityOf(info)
	_, _ = io.WriteString(h, identity.FingerprintString())
}

func captureReloadFingerprints(paths []string) (map[string]Fingerprint, error) {
	fingerprints := make(map[string]Fingerprint, len(paths))
	for _, path := range paths {
		if err := addReloadFingerprint(fingerprints, path); err != nil {
			return nil, err
		}
	}
	return fingerprints, nil
}

func addReloadFingerprint(fingerprints map[string]Fingerprint, path string) error {
	if path == "" {
		return nil
	}
	path = NormalizeReloadPath(path)
	if _, exists := fingerprints[path]; exists {
		return nil
	}
	fingerprint, err := ReadReloadFingerprint(path)
	if err != nil {
		return err
	}
	fingerprints[path] = fingerprint
	return nil
}

func ReadReloadFingerprint(path string) (Fingerprint, error) {
	_, fingerprint, err := ReadStableConfigFile(path)
	if err != nil && !os.IsNotExist(err) {
		return Fingerprint{}, err
	}
	return fingerprint, nil
}

func reloadFingerprintsChanged(before map[string]Fingerprint) bool {
	return ReloadFingerprintsChangedWithOwner(before, nil)
}

func ReloadFingerprintsChangedWithOwner(before map[string]Fingerprint, owner func(string) (int, bool, error)) bool {
	for path, expected := range before {
		expectedOwner, enforceOwner := 0, false
		var ownerErr error
		if owner != nil {
			expectedOwner, enforceOwner, ownerErr = owner(path)
			if ownerErr != nil {
				return true
			}
		}
		_, actual, err := ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
		if err != nil || actual != expected {
			if os.IsNotExist(err) {
				// readStableConfigFileOwned returns the discovery fingerprint along
				// with a not-exist error, so this remains an exact comparison.
				if actual == expected {
					continue
				}
			}
			return true
		}
	}
	return false
}

func SameReloadPathSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, path := range left {
		leftSet[NormalizeReloadPath(path)] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, path := range right {
		rightSet[NormalizeReloadPath(path)] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for path := range rightSet {
		if _, ok := leftSet[path]; !ok {
			return false
		}
	}
	return true
}

// TrackedSlotFingerprint is the single disk-fingerprint accessor for
// tracked slot paths: it returns the captured reload fingerprint when one
// exists for the path (selected documents, plus canonical/normalized
// aliases via reloadFingerprintForPath), and falls back to a REAL on-disk
// fingerprint (raw bytes: existence plus identity/digest, no decoding,
// never applied to the config) for tracked paths the selected set does not
// cover — an unselected slot sibling that exists must not look absent.
func TrackedSlotFingerprint(path string, fingerprints map[string]Fingerprint) (Fingerprint, error) {
	if fingerprint := reloadFingerprintForPath(fingerprints, path); fingerprint != (Fingerprint{}) {
		return fingerprint, nil
	}
	return ReadReloadFingerprint(path)
}

func reloadFingerprintForPath(fingerprints map[string]Fingerprint, path string) Fingerprint {
	if fingerprint, ok := fingerprints[path]; ok {
		return RebindReloadFingerprintPath(path, fingerprint)
	}
	canonical := NormalizeReloadPath(path)
	if fingerprint, ok := fingerprints[canonical]; ok {
		return RebindReloadFingerprintPath(path, fingerprint)
	}
	for candidate, fingerprint := range fingerprints {
		if NormalizeReloadPath(candidate) == canonical {
			return RebindReloadFingerprintPath(path, fingerprint)
		}
	}
	return Fingerprint{}
}

func RebindReloadFingerprintPath(path string, fingerprint Fingerprint) Fingerprint {
	fingerprint.Discovery = ConfigDiscoveryFingerprint(path)
	fingerprint.ParentDiscovery = ConfigDiscoveryFingerprint(filepath.Dir(path))
	fingerprint.ParentIdentity = fileid.ConfigParentIdentity(path)
	fingerprint.AliasChain = ConfigAliasChainFingerprint(path)
	return fingerprint
}
