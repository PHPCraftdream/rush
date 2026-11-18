package mcpstore

// The MCP admission boundary: the guard, its token lifecycle, the on-disk validation that a prepared source is still the one being published, and the currency predicates behind it. Split out of store_mcp.go when the 1000-line file limit landed; store_mcp.go keeps the Persist* mutation surface.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
)

// MCPAdmissionGuard is the final source validation boundary for a runtime
// publication. Its token is prepared while the config sidecars are held, so
// ValidateCurrent is deliberately memory-only and safe under lifecycleMu.
type MCPAdmissionGuard struct {
	store *Store
	// Ref is exported for the root white-box admission tests.
	Ref *AdmissionTokenRef
}

type AdmissionTokenRef struct {
	mu    sync.Mutex
	token *AdmissionToken
}

type AdmissionToken struct {
	store        *Store
	snapshot     MCPAdmissionSnapshot
	name         string
	fingerprints map[string]docread.Fingerprint
	absent       map[string]docread.Fingerprint
	// Handles and Closed are exported for the root white-box admission tests
	// (mcp_admission_io_test.go); the rest of the token state stays internal.
	Handles   map[string]*os.File
	Closed    atomic.Bool
	closeOnce sync.Once
}

func (t *AdmissionToken) close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() {
		for _, file := range t.Handles {
			if file != nil {
				_ = file.Close()
			}
		}
		t.Handles = nil
		t.Closed.Store(true)
	})
}

func (r *AdmissionTokenRef) CurrentToken() *AdmissionToken {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	token := r.token
	r.mu.Unlock()
	return token
}

func (r *AdmissionTokenRef) replace(token *AdmissionToken) {
	if r == nil {
		if token != nil {
			token.close()
		}
		return
	}
	r.mu.Lock()
	previous := r.token
	r.token = token
	r.mu.Unlock()
	previous.close()
}

func (r *AdmissionTokenRef) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	token := r.token
	r.token = nil
	r.mu.Unlock()
	token.close()
}

// ValidateCurrent rejects admission when the immutable prepared token was
// closed or its in-memory generation fence is no longer current. It performs
// no filesystem I/O and is safe while lifecycleMu is held.
func (g MCPAdmissionGuard) ValidateCurrent() error {
	token := g.Ref.CurrentToken()
	if g.store == nil || token == nil || token.store != g.store || token.Closed.Load() {
		return ErrMCPMutationStale
	}
	if !g.store.mcpAdmissionSnapshotCurrent(token.snapshot, token.name) {
		return ErrMCPMutationStale
	}
	return nil
}

// FinalValidateCurrentContext verifies the pinned source bytes and identities
// at the lifecycle publication boundary. It performs bounded filesystem I/O
// and must be called only while the caller's lifecycle critical section is
// held.
func (g MCPAdmissionGuard) FinalValidateCurrentContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	token := g.Ref.CurrentToken()
	if g.store == nil || token == nil || token.store != g.store || token.Closed.Load() {
		return ErrMCPMutationStale
	}
	if !g.store.mcpAdmissionSnapshotCurrent(token.snapshot, token.name) {
		return ErrMCPMutationStale
	}
	if err := g.store.validateMCPAdmissionTokenContext(ctx, token); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !g.store.mcpAdmissionSnapshotCurrent(token.snapshot, token.name) {
		return ErrMCPMutationStale
	}
	return nil
}

// RevalidateCurrent refreshes the pinned token before the lifecycle turn. It
// may perform filesystem I/O and must never be called while lifecycleMu is
// held. ValidateCurrent is the corresponding memory-only lifecycle check.
func (g MCPAdmissionGuard) RevalidateCurrent() error {
	return g.RevalidateCurrentContext(context.Background())
}

// RevalidateCurrentContext is the context-aware form of RevalidateCurrent.
func (g MCPAdmissionGuard) RevalidateCurrentContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	token := g.Ref.CurrentToken()
	if g.store == nil || token == nil || token.store != g.store || token.Closed.Load() {
		return ErrMCPMutationStale
	}
	next, err := g.store.prepareMCPAdmissionToken(ctx, token.snapshot, token.name, token.fingerprints)
	if err != nil {
		return err
	}
	g.Ref.replace(next)
	return nil
}

type mcpCommitUncertainError struct {
	cause error
}

func (e *mcpCommitUncertainError) Error() string {
	return fmt.Sprintf("%s: %v", ErrMCPCommitUncertain, e.cause)
}

func (e *mcpCommitUncertainError) Unwrap() []error {
	return []error{ErrMCPCommitUncertain, e.cause}
}

// MCPCommitWasReconciled is the exported form used by the root white-box
// test forwarder (mcpCommitWasReconciled).
func MCPCommitWasReconciled(err error) bool {
	outcome, ok := cfgcommit.CommitOutcomeFromError(err)
	return ok && outcome.Committed && outcome.Reconciled
}

// MCPMutationResult describes the effective configuration on both sides of a
// durable MCP mutation. NewConfig/NewOrigin may describe a lower-priority
// definition revealed by removing or replacing the old one.
type MCPMutationResult struct {
	Operation string
	OldName   string
	NewName   string
	// Generation identifies the store snapshot published for this mutation.
	// Lifecycle callers must not use a result after a newer snapshot exists.
	Generation            uint64
	OldExists             bool
	NewExists             bool
	OldConfig             MCPConfig
	NewConfig             MCPConfig
	OldOrigin             MCPOrigin
	NewOrigin             MCPOrigin
	FallbackExists        bool
	FallbackConfig        MCPConfig
	FallbackOrigin        MCPOrigin
	committedFingerprints map[string]docread.Fingerprint
	committedMCPInputs    map[string][32]byte
}

// WithCurrentMCPMutation validates result against a fresh evaluation of the
// locked config files, then runs fn while that evaluation remains pinned.
//
// fn is the runtime publication callback only: it must not mutate config on
// disk or re-enter ConfigStore persistence. It may block on lifecycle locks;
// the shared config sidecar locks stay held until it returns.
func (s *Store) WithCurrentMCPMutation(result MCPMutationResult, fn func() error) error {
	return s.port.PublishMutation(func(MutationScope) error {
		return s.withMCPWriteLocks(func(files *LockedFiles) error {
			evaluation, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return ErrMCPMutationStale
			}
			if !s.mcpMutationResultCurrentLocked(result, evaluation) {
				return ErrMCPMutationStale
			}
			return fn()
		})
	})
}

// WithCurrentMCPAdmission validates an immutable MCP admission snapshot and
// runs fn while the config snapshot and disk inputs remain pinned.
func (s *Store) WithCurrentMCPAdmission(snapshot MCPAdmissionSnapshot, name string, fn func(MCPAdmissionGuard) error) error {
	ctx, cancel := cfgcommit.ConfigContextWithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.WithCurrentMCPAdmissionContext(ctx, snapshot, name, fn)
}

// WithCurrentMCPAdmissionContext prepares and validates the source token
// before invoking fn. The caller context is used for every sidecar lock wait;
// fn is the only callback that may run while the locks are held.
func (s *Store) WithCurrentMCPAdmissionContext(ctx context.Context, snapshot MCPAdmissionSnapshot, name string, fn func(MCPAdmissionGuard) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.port.PublishMutation(func(MutationScope) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.port.WorkingDir() == "" && s.port.GlobalDataPath() == "" {
			if !s.mcpAdmissionSnapshotCurrent(snapshot, name) {
				return ErrMCPMutationStale
			}
			token := &AdmissionToken{store: s, snapshot: snapshot, name: name}
			ref := &AdmissionTokenRef{token: token}
			defer ref.close()
			return fn(MCPAdmissionGuard{store: s, Ref: ref})
		}
		err := s.withMCPAdmissionLocksContext(ctx, func(files *LockedFiles) error {
			if !s.mcpAdmissionSnapshotCurrent(snapshot, name) {
				return ErrMCPMutationStale
			}
			evaluation, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return ErrMCPMutationStale
			}
			input, hasInput := evaluation.MCPInputs[name]
			if hasInput != snapshot.HasMCPInput || hasInput && input != snapshot.MCPInput {
				return ErrMCPMutationStale
			}
			token, tokenErr := s.prepareMCPAdmissionToken(ctx, snapshot, name, evaluation.Fingerprints)
			if tokenErr != nil {
				return tokenErr
			}
			ref := &AdmissionTokenRef{token: token}
			defer ref.close()
			return fn(MCPAdmissionGuard{store: s, Ref: ref})
		})
		if errors.Is(err, ErrMCPStale) {
			return fmt.Errorf("%w: %w", ErrMCPMutationStale, err)
		}
		return err
	})
}

func (s *Store) prepareMCPAdmissionToken(ctx context.Context, snapshot MCPAdmissionSnapshot, name string, fingerprints map[string]docread.Fingerprint) (*AdmissionToken, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	token := &AdmissionToken{
		store: s, snapshot: snapshot, name: name,
		fingerprints: cloneReloadFingerprints(fingerprints),
		absent:       make(map[string]docread.Fingerprint),
		Handles:      make(map[string]*os.File),
	}
	for path, fingerprint := range token.fingerprints {
		if err := ctx.Err(); err != nil {
			token.close()
			return nil, err
		}
		if !fingerprint.Exists {
			absence, err := stableMCPAdmissionAbsence(ctx, path, fingerprint)
			if err != nil {
				token.close()
				return nil, err
			}
			token.absent[path] = absence
			if err := ctx.Err(); err != nil {
				token.close()
				return nil, err
			}
			continue
		}
		expectedOwner, enforceOwner, ownerErr := s.mcpOwnerPolicy(path)
		if ownerErr != nil {
			token.close()
			return nil, ownerErr
		}
		if err := ctx.Err(); err != nil {
			token.close()
			return nil, err
		}
		file, err := fileid.OpenStableConfigFile(path)
		if err != nil {
			token.close()
			return nil, fmt.Errorf("%w: failed to prepare MCP admission source %s: %v", ErrMCPMutationStale, path, err)
		}
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			token.close()
			return nil, err
		}
		if err := ValidatePreparedAdmissionFile(ctx, path, file, fingerprint, expectedOwner, enforceOwner); err != nil {
			_ = file.Close()
			token.close()
			return nil, err
		}
		token.Handles[path] = file
		if err := ctx.Err(); err != nil {
			token.close()
			return nil, err
		}
	}
	return token, nil
}

func stableMCPAdmissionAbsence(ctx context.Context, path string, expected docread.Fingerprint) (docread.Fingerprint, error) {
	if err := ctx.Err(); err != nil {
		return docread.Fingerprint{}, err
	}
	first, err := mcpAdmissionAbsenceFingerprint(path)
	if err != nil {
		return docread.Fingerprint{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return docread.Fingerprint{}, contextErr
	}
	second, err := mcpAdmissionAbsenceFingerprint(path)
	if err != nil {
		return docread.Fingerprint{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return docread.Fingerprint{}, contextErr
	}
	if first != second || second != expected {
		return docread.Fingerprint{}, ErrMCPMutationStale
	}
	return second, nil
}

func mcpAdmissionAbsenceFingerprint(path string) (docread.Fingerprint, error) {
	if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
		return docread.Fingerprint{}, ErrMCPMutationStale
	}
	return docread.Fingerprint{
		Discovery:       docread.ConfigDiscoveryFingerprint(path),
		ParentDiscovery: docread.ConfigDiscoveryFingerprint(filepath.Dir(path)),
		ParentIdentity:  fileid.ConfigParentIdentity(path),
		AliasChain:      docread.ConfigAliasChainFingerprint(path),
	}, nil
}

// ValidatePreparedAdmissionFile is the exported form used by the root
// white-box admission test forwarder (validatePreparedAdmissionFile).
func ValidatePreparedAdmissionFile(ctx context.Context, path string, file *os.File, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrMCPMutationStale, fileid.ErrConfigNonRegular)
	}
	owner, ownerKnown := fileid.ConfigFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return ErrMCPMutationStale
	}
	first, err := fileid.ReadOpenedConfigBytes(file)
	if err != nil {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	second, err := fileid.ReadOpenedConfigBytes(file)
	if err != nil || !bytes.Equal(first, second) {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalInfo, err := file.Stat()
	if err != nil || !finalInfo.Mode().IsRegular() {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, ownerKnown = fileid.ConfigFileOwner(finalInfo)
	actualNlink := fileid.FileNlinkOfOpened(file, finalInfo)
	if (expected.Identity.Valid || enforceOwner) && (!ownerKnown || owner != expected.Owner) ||
		expected.Nlink != 0 && actualNlink != expected.Nlink ||
		expected.Size != int64(len(second)) || expected.ModTime != finalInfo.ModTime().UnixNano() ||
		expected.Digest != sha256.Sum256(second) ||
		expected.Discovery != docread.ConfigDiscoveryFingerprint(path) ||
		expected.ParentDiscovery != docread.ConfigDiscoveryFingerprint(filepath.Dir(path)) ||
		expected.ParentIdentity != fileid.ConfigParentIdentity(path) ||
		expected.AliasChain != docread.ConfigAliasChainFingerprint(path) {
		return ErrMCPMutationStale
	}
	pathMatches, err := fileid.ConfigFilePathIdentityMatches(path, file, finalInfo)
	if err != nil || !pathMatches {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// MCPAdmissionVerifyBufferSize is the exported form used by the root
// white-box admission test (MCPAdmissionVerifyBufferSize).
const MCPAdmissionVerifyBufferSize = 32 << 10

// AdmissionReadSeeker is the exported form used by the root white-box
// admission test forwarder (mcpAdmissionReadSeeker).
type AdmissionReadSeeker interface {
	io.Reader
	io.Seeker
}

// verifyMCPAdmissionBytes hashes exactly the expected size and then probes for
// one extra byte. Memory use is independent of the file size.
// VerifyMCPAdmissionBytes is the exported form used by the root white-box
// admission test forwarder (verifyMCPAdmissionBytes).
func VerifyMCPAdmissionBytes(ctx context.Context, file AdmissionReadSeeker, expectedSize int64, expectedDigest [sha256.Size]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expectedSize < 0 {
		return ErrMCPMutationStale
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	hash := sha256.New()
	var buffer [MCPAdmissionVerifyBufferSize]byte
	remaining := expectedSize
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		request := int64(len(buffer))
		if remaining < request {
			request = remaining
		}
		read, err := file.Read(buffer[:request])
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			remaining -= int64(read)
		}
		if err != nil {
			return ErrMCPMutationStale
		}
		if read == 0 {
			return ErrMCPMutationStale
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var extra [1]byte
	read, err := file.Read(extra[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return ErrMCPMutationStale
	}
	if read != 0 {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var actualDigest [sha256.Size]byte
	copy(actualDigest[:], hash.Sum(nil))
	if actualDigest != expectedDigest {
		return ErrMCPMutationStale
	}
	return nil
}

// ValidateFinalMCPAdmissionFile is the exported form used by the root
// white-box admission test forwarder (validateFinalMCPAdmissionFile).
func ValidateFinalMCPAdmissionFile(
	ctx context.Context,
	path string,
	file *os.File,
	expected docread.Fingerprint,
	expectedOwner int,
	enforceOwner bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if file == nil || !expected.Exists {
		return ErrMCPMutationStale
	}
	info, err := file.Stat()
	if err != nil {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != expected.Size {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", ErrMCPMutationStale, fileid.ErrConfigNonRegular)
		}
		return ErrMCPMutationStale
	}
	owner, ownerKnown := fileid.ConfigFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return ErrMCPMutationStale
	}
	identity := fileid.FileIdentityOfOpened(file, info)
	if expected.Identity.Valid && identity != expected.Identity {
		return ErrMCPMutationStale
	}
	if expected.Nlink != 0 && fileid.FileNlinkOfOpened(file, info) != expected.Nlink {
		return ErrMCPMutationStale
	}
	if err := VerifyMCPAdmissionBytes(ctx, file, expected.Size, expected.Digest); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalInfo, err := file.Stat()
	if err != nil || !finalInfo.Mode().IsRegular() || finalInfo.Size() != expected.Size {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalOwner, finalOwnerKnown := fileid.ConfigFileOwner(finalInfo)
	finalIdentity := fileid.FileIdentityOfOpened(file, finalInfo)
	finalNlink := fileid.FileNlinkOfOpened(file, finalInfo)
	if (expected.Identity.Valid || enforceOwner) && (!finalOwnerKnown || finalOwner != expected.Owner) ||
		expected.Identity.Valid && finalIdentity != expected.Identity ||
		expected.Nlink != 0 && finalNlink != expected.Nlink ||
		expected.ModTime != finalInfo.ModTime().UnixNano() ||
		expected.Discovery != docread.ConfigDiscoveryFingerprint(path) ||
		expected.ParentDiscovery != docread.ConfigDiscoveryFingerprint(filepath.Dir(path)) ||
		expected.ParentIdentity != fileid.ConfigParentIdentity(path) ||
		expected.AliasChain != docread.ConfigAliasChainFingerprint(path) {
		return ErrMCPMutationStale
	}
	pathMatches, err := fileid.ConfigFilePathIdentityMatches(path, file, finalInfo)
	if err != nil || !pathMatches {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (s *Store) validateMCPAdmissionTokenContext(ctx context.Context, token *AdmissionToken) error {
	paths := make([]string, 0, len(token.fingerprints))
	for path := range token.fingerprints {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if token.Closed.Load() {
			return ErrMCPMutationStale
		}
		expected := token.fingerprints[path]
		if !expected.Exists {
			absence, err := mcpAdmissionAbsenceFingerprint(path)
			if err != nil {
				return err
			}
			if absence != token.absent[path] {
				return ErrMCPMutationStale
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		file := token.Handles[path]
		expectedOwner, enforceOwner, err := s.mcpOwnerPolicy(path)
		if err != nil {
			return err
		}
		if err := ValidateFinalMCPAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner); err != nil {
			return err
		}
	}
	return nil
}

func cloneReloadFingerprints(source map[string]docread.Fingerprint) map[string]docread.Fingerprint {
	if source == nil {
		return nil
	}
	result := make(map[string]docread.Fingerprint, len(source))
	for path, fingerprint := range source {
		result[path] = fingerprint
	}
	return result
}

func (s *Store) mcpAdmissionSnapshotCurrent(snapshot MCPAdmissionSnapshot, name string) bool {
	var current MCPView
	_ = s.port.ViewSnap(func(view MCPView) error {
		current = view
		return nil
	})
	if current.Generation() < snapshot.Generation ||
		current.MCPRevisions()[name] != snapshot.MCPRevision ||
		current.ResolverRevision() != snapshot.ResolverRevision ||
		current.Config() == nil {
		return false
	}
	value, exists := current.Config().MCP[name]
	return exists == snapshot.Exists && (!exists || reflect.DeepEqual(value, snapshot.MCPConfig))
}

func (s *Store) mcpMutationResultCurrentLocked(result MCPMutationResult, evaluation Evaluation) bool {
	if result.Generation != 0 && s.currentGeneration() != result.Generation {
		return false
	}

	// The result records every exact spelling changed by its commit. Checking
	// these bytes while the sidecars are held closes the release-and-recheck
	// window that allowed another ConfigStore to replace the commit with an
	// ABA-equivalent effective value.
	for path, expected := range result.committedFingerprints {
		actual, ok := mcpEvaluationFingerprint(evaluation.Fingerprints, path)
		if !ok || actual != expected {
			return false
		}
	}

	// Preserve the old staleness fence for unrelated tracked inputs too. This
	// also supports compatibility results assembled by lifecycle adapters that
	// predate committedFingerprints.
	var snapshot MCPView
	_ = s.port.ViewSnap(func(view MCPView) error {
		snapshot = view
		return nil
	})
	for path, expected := range snapshot.Snapshots() {
		if expected.Fingerprint() == (docread.Fingerprint{}) {
			continue
		}
		actual, ok := mcpEvaluationFingerprint(evaluation.Fingerprints, path)
		if !ok {
			// Slot-sibling expansion tracks BOTH spellings of every logical
			// slot, including a sibling the MCP evaluation never reads
			// because it does not exist (an absent rush.ktav beside an
			// active rush.json). An absent tracked file the evaluation does
			// not cover cannot make the mutation stale. An EXISTING one (the
			// legacy rush.json kept beside the selected rush.ktav after
			// migration) is fingerprinted from its real disk bytes, exactly
			// as the snapshot builder and ConfigStaleness do: it is stale
			// only if those bytes changed, not merely because the
			// evaluation does not read it.
			if !expected.Exists {
				continue
			}
			sibling, err := docread.TrackedSlotFingerprint(path, nil)
			if err != nil || !reloadFingerprintContentEqual(expected.Fingerprint(), sibling) {
				return false
			}
			continue
		}
		if !reloadFingerprintContentEqual(expected.Fingerprint(), actual) {
			return false
		}
	}

	current, exists := evaluation.Configs[result.NewName]
	if exists != result.NewExists || exists && !reflect.DeepEqual(current, result.NewConfig) {
		return false
	}
	if exists && result.NewOrigin != (MCPOrigin{}) && evaluation.Origins[result.NewName] != result.NewOrigin {
		return false
	}
	if result.Operation == "replace" && result.OldName != result.NewName {
		fallback, fallbackExists := evaluation.Configs[result.OldName]
		if fallbackExists != result.FallbackExists || fallbackExists && !reflect.DeepEqual(fallback, result.FallbackConfig) {
			return false
		}
		if fallbackExists && result.FallbackOrigin != (MCPOrigin{}) && evaluation.Origins[result.OldName] != result.FallbackOrigin {
			return false
		}
	}
	return true
}

func reloadFingerprintContentEqual(expected, actual docread.Fingerprint) bool {
	if expected.Exists != actual.Exists || expected.Size != actual.Size || expected.Digest != actual.Digest {
		return false
	}
	if !expected.Exists {
		return true
	}
	return expected == actual
}

func committedMCPFingerprints(files *LockedFiles) map[string]docread.Fingerprint {
	result := make(map[string]docread.Fingerprint)
	for _, record := range files.Records {
		if !files.Changed[record.Key] {
			continue
		}
		for alias := range record.Aliases {
			if fingerprint, ok := files.Fingerprints[alias]; ok {
				result[alias] = fingerprint
			}
		}
		if fingerprint, ok := files.Fingerprints[record.CommitPath]; ok {
			result[record.CommitPath] = fingerprint
		}
	}
	return result
}
