package config

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
)

// MCPAdmissionGuard is the final source validation boundary for a runtime
// publication. Its token is prepared while the config sidecars are held, so
// ValidateCurrent is deliberately memory-only and safe under lifecycleMu.
type MCPAdmissionGuard struct {
	store *ConfigStore
	ref   *mcpAdmissionTokenRef
}

type mcpAdmissionTokenRef struct {
	mu    sync.Mutex
	token *mcpAdmissionToken
}

type mcpAdmissionToken struct {
	store        *ConfigStore
	snapshot     MCPAdmissionSnapshot
	name         string
	fingerprints map[string]reloadFileFingerprint
	absent       map[string]reloadFileFingerprint
	handles      map[string]*os.File
	closeOnce    sync.Once
	closed       atomic.Bool
}

func (t *mcpAdmissionToken) close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() {
		for _, file := range t.handles {
			if file != nil {
				_ = file.Close()
			}
		}
		t.handles = nil
		t.closed.Store(true)
	})
}

func (r *mcpAdmissionTokenRef) current() *mcpAdmissionToken {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	token := r.token
	r.mu.Unlock()
	return token
}

func (r *mcpAdmissionTokenRef) replace(token *mcpAdmissionToken) {
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

func (r *mcpAdmissionTokenRef) close() {
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
	token := g.ref.current()
	if g.store == nil || token == nil || token.store != g.store || token.closed.Load() {
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
	token := g.ref.current()
	if g.store == nil || token == nil || token.store != g.store || token.closed.Load() {
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
	token := g.ref.current()
	if g.store == nil || token == nil || token.store != g.store || token.closed.Load() {
		return ErrMCPMutationStale
	}
	next, err := g.store.prepareMCPAdmissionToken(ctx, token.snapshot, token.name, token.fingerprints)
	if err != nil {
		return err
	}
	g.ref.replace(next)
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

func mcpCommitWasReconciled(err error) bool {
	outcome, ok := CommitOutcomeFromError(err)
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
	committedFingerprints map[string]reloadFileFingerprint
	committedMCPInputs    map[string][32]byte
}

// WithCurrentMCPMutation validates result against a fresh evaluation of the
// locked config files, then runs fn while that evaluation remains pinned.
//
// fn is the runtime publication callback only: it must not mutate config on
// disk or re-enter ConfigStore persistence. It may block on lifecycle locks;
// the shared config sidecar locks stay held until it returns.
func (s *ConfigStore) WithCurrentMCPMutation(result MCPMutationResult, fn func() error) error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		evaluation, err := s.evaluateMCPFiles(files)
		if err != nil {
			return ErrMCPMutationStale
		}
		if !s.mcpMutationResultCurrentLocked(result, evaluation) {
			return ErrMCPMutationStale
		}
		return fn()
	})
}

// WithCurrentMCPAdmission validates an immutable MCP admission snapshot and
// runs fn while the config snapshot and disk inputs remain pinned.
func (s *ConfigStore) WithCurrentMCPAdmission(snapshot MCPAdmissionSnapshot, name string, fn func(MCPAdmissionGuard) error) error {
	ctx, cancel := configContextWithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.WithCurrentMCPAdmissionContext(ctx, snapshot, name, fn)
}

// WithCurrentMCPAdmissionContext prepares and validates the source token
// before invoking fn. The caller context is used for every sidecar lock wait;
// fn is the only callback that may run while the locks are held.
func (s *ConfigStore) WithCurrentMCPAdmissionContext(ctx context.Context, snapshot MCPAdmissionSnapshot, name string, fn func(MCPAdmissionGuard) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.workingDir == "" && s.globalDataPath == "" {
		if !s.mcpAdmissionSnapshotCurrent(snapshot, name) {
			return ErrMCPMutationStale
		}
		token := &mcpAdmissionToken{store: s, snapshot: snapshot, name: name}
		ref := &mcpAdmissionTokenRef{token: token}
		defer ref.close()
		return fn(MCPAdmissionGuard{store: s, ref: ref})
	}
	err := s.withMCPAdmissionLocksContext(ctx, func(files *mcpLockedFiles) error {
		if !s.mcpAdmissionSnapshotCurrent(snapshot, name) {
			return ErrMCPMutationStale
		}
		evaluation, err := s.evaluateMCPFiles(files)
		if err != nil {
			return ErrMCPMutationStale
		}
		input, hasInput := evaluation.mcpInputs[name]
		if hasInput != snapshot.HasMCPInput || hasInput && input != snapshot.MCPInput {
			return ErrMCPMutationStale
		}
		token, tokenErr := s.prepareMCPAdmissionToken(ctx, snapshot, name, evaluation.fingerprints)
		if tokenErr != nil {
			return tokenErr
		}
		ref := &mcpAdmissionTokenRef{token: token}
		defer ref.close()
		return fn(MCPAdmissionGuard{store: s, ref: ref})
	})
	if errors.Is(err, ErrMCPStale) {
		return fmt.Errorf("%w: %w", ErrMCPMutationStale, err)
	}
	return err
}

func (s *ConfigStore) prepareMCPAdmissionToken(ctx context.Context, snapshot MCPAdmissionSnapshot, name string, fingerprints map[string]reloadFileFingerprint) (*mcpAdmissionToken, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	token := &mcpAdmissionToken{
		store: s, snapshot: snapshot, name: name,
		fingerprints: cloneReloadFingerprints(fingerprints),
		absent:       make(map[string]reloadFileFingerprint),
		handles:      make(map[string]*os.File),
	}
	for path, fingerprint := range token.fingerprints {
		if err := ctx.Err(); err != nil {
			token.close()
			return nil, err
		}
		if !fingerprint.exists {
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
		file, err := openStableConfigFile(path)
		if err != nil {
			token.close()
			return nil, fmt.Errorf("%w: failed to prepare MCP admission source %s: %v", ErrMCPMutationStale, path, err)
		}
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			token.close()
			return nil, err
		}
		if err := validatePreparedAdmissionFile(ctx, path, file, fingerprint, expectedOwner, enforceOwner); err != nil {
			_ = file.Close()
			token.close()
			return nil, err
		}
		token.handles[path] = file
		if err := ctx.Err(); err != nil {
			token.close()
			return nil, err
		}
	}
	return token, nil
}

func stableMCPAdmissionAbsence(ctx context.Context, path string, expected reloadFileFingerprint) (reloadFileFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return reloadFileFingerprint{}, err
	}
	first, err := mcpAdmissionAbsenceFingerprint(path)
	if err != nil {
		return reloadFileFingerprint{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return reloadFileFingerprint{}, contextErr
	}
	second, err := mcpAdmissionAbsenceFingerprint(path)
	if err != nil {
		return reloadFileFingerprint{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return reloadFileFingerprint{}, contextErr
	}
	if first != second || second != expected {
		return reloadFileFingerprint{}, ErrMCPMutationStale
	}
	return second, nil
}

func mcpAdmissionAbsenceFingerprint(path string) (reloadFileFingerprint, error) {
	if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
		return reloadFileFingerprint{}, ErrMCPMutationStale
	}
	return reloadFileFingerprint{
		discovery:       configDiscoveryFingerprint(path),
		parentDiscovery: configDiscoveryFingerprint(filepath.Dir(path)),
		parentIdentity:  configParentIdentity(path),
		aliasChain:      configAliasChainFingerprint(path),
	}, nil
}

func validatePreparedAdmissionFile(ctx context.Context, path string, file *os.File, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool) error {
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
		return fmt.Errorf("%w: %s", ErrMCPMutationStale, ErrConfigNonRegular)
	}
	owner, ownerKnown := configFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return ErrMCPMutationStale
	}
	first, err := readOpenedConfigBytes(file)
	if err != nil {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	second, err := readOpenedConfigBytes(file)
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
	owner, ownerKnown = configFileOwner(finalInfo)
	actualNlink := configFileNlinkOfOpened(file, finalInfo)
	if (expected.identity.valid || enforceOwner) && (!ownerKnown || owner != expected.owner) ||
		expected.nlink != 0 && actualNlink != expected.nlink ||
		expected.size != int64(len(second)) || expected.modTime != finalInfo.ModTime().UnixNano() ||
		expected.digest != sha256.Sum256(second) ||
		expected.discovery != configDiscoveryFingerprint(path) ||
		expected.parentDiscovery != configDiscoveryFingerprint(filepath.Dir(path)) ||
		expected.parentIdentity != configParentIdentity(path) ||
		expected.aliasChain != configAliasChainFingerprint(path) {
		return ErrMCPMutationStale
	}
	pathMatches, err := configFilePathIdentityMatches(path, file, finalInfo)
	if err != nil || !pathMatches {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

const mcpAdmissionVerifyBufferSize = 32 << 10

type mcpAdmissionReadSeeker interface {
	io.Reader
	io.Seeker
}

// verifyMCPAdmissionBytes hashes exactly the expected size and then probes for
// one extra byte. Memory use is independent of the file size.
func verifyMCPAdmissionBytes(ctx context.Context, file mcpAdmissionReadSeeker, expectedSize int64, expectedDigest [sha256.Size]byte) error {
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
	var buffer [mcpAdmissionVerifyBufferSize]byte
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

func validateFinalMCPAdmissionFile(
	ctx context.Context,
	path string,
	file *os.File,
	expected reloadFileFingerprint,
	expectedOwner int,
	enforceOwner bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if file == nil || !expected.exists {
		return ErrMCPMutationStale
	}
	info, err := file.Stat()
	if err != nil {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != expected.size {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", ErrMCPMutationStale, ErrConfigNonRegular)
		}
		return ErrMCPMutationStale
	}
	owner, ownerKnown := configFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return ErrMCPMutationStale
	}
	identity := configFileIdentityOfOpened(file, info)
	if expected.identity.valid && identity != expected.identity {
		return ErrMCPMutationStale
	}
	if expected.nlink != 0 && configFileNlinkOfOpened(file, info) != expected.nlink {
		return ErrMCPMutationStale
	}
	if err := verifyMCPAdmissionBytes(ctx, file, expected.size, expected.digest); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalInfo, err := file.Stat()
	if err != nil || !finalInfo.Mode().IsRegular() || finalInfo.Size() != expected.size {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalOwner, finalOwnerKnown := configFileOwner(finalInfo)
	finalIdentity := configFileIdentityOfOpened(file, finalInfo)
	finalNlink := configFileNlinkOfOpened(file, finalInfo)
	if (expected.identity.valid || enforceOwner) && (!finalOwnerKnown || finalOwner != expected.owner) ||
		expected.identity.valid && finalIdentity != expected.identity ||
		expected.nlink != 0 && finalNlink != expected.nlink ||
		expected.modTime != finalInfo.ModTime().UnixNano() ||
		expected.discovery != configDiscoveryFingerprint(path) ||
		expected.parentDiscovery != configDiscoveryFingerprint(filepath.Dir(path)) ||
		expected.parentIdentity != configParentIdentity(path) ||
		expected.aliasChain != configAliasChainFingerprint(path) {
		return ErrMCPMutationStale
	}
	pathMatches, err := configFilePathIdentityMatches(path, file, finalInfo)
	if err != nil || !pathMatches {
		return ErrMCPMutationStale
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (s *ConfigStore) validateMCPAdmissionTokenContext(ctx context.Context, token *mcpAdmissionToken) error {
	paths := make([]string, 0, len(token.fingerprints))
	for path := range token.fingerprints {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if token.closed.Load() {
			return ErrMCPMutationStale
		}
		expected := token.fingerprints[path]
		if !expected.exists {
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
		file := token.handles[path]
		expectedOwner, enforceOwner, err := s.mcpOwnerPolicy(path)
		if err != nil {
			return err
		}
		if err := validateFinalMCPAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner); err != nil {
			return err
		}
	}
	return nil
}

func cloneReloadFingerprints(source map[string]reloadFileFingerprint) map[string]reloadFileFingerprint {
	if source == nil {
		return nil
	}
	result := make(map[string]reloadFileFingerprint, len(source))
	for path, fingerprint := range source {
		result[path] = fingerprint
	}
	return result
}

func (s *ConfigStore) mcpAdmissionSnapshotCurrent(snapshot MCPAdmissionSnapshot, name string) bool {
	current := s.loadSnapshot()
	if current.generation < snapshot.Generation ||
		current.mcpRevisions[name] != snapshot.MCPRevision ||
		current.resolverRevision != snapshot.ResolverRevision ||
		current.config == nil {
		return false
	}
	value, exists := current.config.MCP[name]
	return exists == snapshot.Exists && (!exists || reflect.DeepEqual(value, snapshot.MCPConfig))
}

func (s *ConfigStore) mcpMutationResultCurrentLocked(result MCPMutationResult, evaluation mcpEvaluation) bool {
	if result.Generation != 0 && s.loadSnapshot().generation != result.Generation {
		return false
	}

	// The result records every exact spelling changed by its commit. Checking
	// these bytes while the sidecars are held closes the release-and-recheck
	// window that allowed another ConfigStore to replace the commit with an
	// ABA-equivalent effective value.
	for path, expected := range result.committedFingerprints {
		actual, ok := mcpEvaluationFingerprint(evaluation.fingerprints, path)
		if !ok || actual != expected {
			return false
		}
	}

	// Preserve the old staleness fence for unrelated tracked inputs too. This
	// also supports compatibility results assembled by lifecycle adapters that
	// predate committedFingerprints.
	snapshot := s.loadSnapshot()
	for path, expected := range snapshot.snapshots {
		if expected.fingerprint == (reloadFileFingerprint{}) {
			continue
		}
		actual, ok := mcpEvaluationFingerprint(evaluation.fingerprints, path)
		if !ok || !reloadFingerprintContentEqual(expected.fingerprint, actual) {
			return false
		}
	}

	current, exists := evaluation.configs[result.NewName]
	if exists != result.NewExists || exists && !reflect.DeepEqual(current, result.NewConfig) {
		return false
	}
	if exists && result.NewOrigin != (MCPOrigin{}) && evaluation.origins[result.NewName] != result.NewOrigin {
		return false
	}
	if result.Operation == "replace" && result.OldName != result.NewName {
		fallback, fallbackExists := evaluation.configs[result.OldName]
		if fallbackExists != result.FallbackExists || fallbackExists && !reflect.DeepEqual(fallback, result.FallbackConfig) {
			return false
		}
		if fallbackExists && result.FallbackOrigin != (MCPOrigin{}) && evaluation.origins[result.OldName] != result.FallbackOrigin {
			return false
		}
	}
	return true
}

func reloadFingerprintContentEqual(expected, actual reloadFileFingerprint) bool {
	if expected.exists != actual.exists || expected.size != actual.size || expected.digest != actual.digest {
		return false
	}
	if !expected.exists {
		return true
	}
	return expected == actual
}

func committedMCPFingerprints(files *mcpLockedFiles) map[string]reloadFileFingerprint {
	result := make(map[string]reloadFileFingerprint)
	for _, record := range files.records {
		if !files.changed[record.key] {
			continue
		}
		for alias := range record.aliases {
			if fingerprint, ok := files.fingerprints[alias]; ok {
				result[alias] = fingerprint
			}
		}
		if fingerprint, ok := files.fingerprints[record.commitPath]; ok {
			result[record.commitPath] = fingerprint
		}
	}
	return result
}
