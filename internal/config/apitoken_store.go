package config

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
)

const (
	apiTokenStoreVersion = 1
	apiTokenFileName     = "api-tokens.json"
	apiTokenKtavFileName = "api-tokens.ktav"
)

var (
	apiTokenIDPattern                               = regexp.MustCompile(`^[0-9a-f]{8}$`)
	apiTokenSecretPattern                           = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	apiTokenCompareHook    func([]byte, []byte) int = subtle.ConstantTimeCompare
	apiTokenBeforeLockHook func(*APITokenStore)
	apiTokenWriteFile      = atomicWriteFile
)

var errInvalidAPIToken = errors.New("invalid API token")

// ErrAPITokenNotFound is returned when an API token id does not exist.
var ErrAPITokenNotFound = errors.New("API token not found")

// APIToken is the persisted authorization record. Hash is SHA-256 of the full
// plaintext credential; plaintext credentials are never persisted.
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Permission string     `json:"permission"`
	Created    time.Time  `json:"created"`
	Expires    *time.Time `json:"expires,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Hash       string     `json:"-"`
}

func (t APIToken) clone() APIToken {
	copy := t
	copy.Scopes = append([]string(nil), t.Scopes...)
	if t.Expires != nil {
		expires := *t.Expires
		copy.Expires = &expires
	}
	if t.RevokedAt != nil {
		revokedAt := *t.RevokedAt
		copy.RevokedAt = &revokedAt
	}
	return copy
}

// String intentionally omits the credential digest and all secret material.
func (t APIToken) String() string {
	return fmt.Sprintf("APIToken{ID:%s Name:%q Scopes:%v Permission:%s}", t.ID, t.Name, t.Scopes, t.Permission)
}

func (t APIToken) GoString() string                  { return t.String() }
func (t APIToken) Format(state fmt.State, verb rune) { _, _ = fmt.Fprint(state, t.String()) }

type apiTokenRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Permission string     `json:"permission"`
	Created    time.Time  `json:"created"`
	Expires    *time.Time `json:"expires,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Hash       string     `json:"sha256"`
}

func toAPIRecord(t APIToken) apiTokenRecord {
	return apiTokenRecord(t)
}

func (t *APIToken) UnmarshalJSON(data []byte) error {
	var r apiTokenRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	t.ID, t.Name, t.Scopes, t.Permission, t.Created, t.Expires, t.RevokedAt, t.Hash = r.ID, r.Name, r.Scopes, r.Permission, r.Created, r.Expires, r.RevokedAt, r.Hash
	return nil
}

func (t APIToken) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID         string     `json:"id"`
		Name       string     `json:"name"`
		Scopes     []string   `json:"scopes"`
		Permission string     `json:"permission"`
		Created    time.Time  `json:"created"`
		Expires    *time.Time `json:"expires,omitempty"`
		RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	}{t.ID, t.Name, t.Scopes, t.Permission, t.Created, t.Expires, t.RevokedAt})
}

type apiTokenDocument struct {
	Version int        `json:"version"`
	Tokens  []APIToken `json:"tokens"`
}

func (d apiTokenDocument) MarshalJSON() ([]byte, error) {
	tokens := make([]apiTokenRecord, len(d.Tokens))
	for i := range d.Tokens {
		tokens[i] = toAPIRecord(d.Tokens[i])
	}
	return json.Marshal(struct {
		Version int              `json:"version"`
		Tokens  []apiTokenRecord `json:"tokens"`
	}{d.Version, tokens})
}

// APITokenStore is deliberately independent of ConfigStore reload and project scopes.
type APITokenStore struct {
	Path              string
	mu                sync.Mutex
	cacheMu           sync.Mutex
	cached            apiTokenDocument
	cachedFingerprint tokenFingerprint
	cacheValid        bool
	statHook          func() // test instrumentation; called when a fingerprint changes
	readHook          func() // test instrumentation; called only when bytes are read
	mutateHook        func() // test instrumentation; called while the write lock is held after read
	usedMu            sync.Mutex
	flushMu           sync.Mutex
	lastUsed          map[string]time.Time
	lastFlush         time.Time
	bootstrapMu       sync.Mutex
	bootstrap         *APIToken
	bootstrapConsumed bool
	bootstrapResult   APIToken
	bootstrapOK       bool
	bootstrapErr      error
}
type tokenFingerprint struct {
	size     int64
	modified time.Time
	digest   [sha256.Size]byte
	identity string
}

func GlobalAPITokenStore() *APITokenStore {
	return &APITokenStore{Path: filepath.Join(filepath.Dir(GlobalConfigData()), apiTokenFileName)}
}

// CreateAPIToken generates and durably records a rush_<id>.<secret> credential,
// returning the plaintext exactly once to its caller.
func (s *APITokenStore) CreateAPIToken(name string, scopes []string, permission string, expires *time.Time) (string, APIToken, error) {
	if strings.TrimSpace(name) == "" {
		return "", APIToken{}, errors.New("token name is required")
	}
	if len(scopes) == 0 {
		return "", APIToken{}, errors.New("at least one scope is required")
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if scope != "read" && scope != "run" && scope != "config" && scope != "admin" {
			return "", APIToken{}, errors.New("invalid API token scope")
		}
		seen[scope] = true
	}
	if permission == "" {
		permission = "ask"
	}
	if permission != "ask" && permission != "readonly" && permission != "auto" {
		return "", APIToken{}, errors.New("invalid API token permission")
	}
	if permission == "auto" && !seen["admin"] {
		return "", APIToken{}, errors.New("auto permission requires admin scope")
	}
	secretBytes := make([]byte, 32)
	idBytes := make([]byte, 4)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", APIToken{}, err
	}
	if _, err := rand.Read(idBytes); err != nil {
		return "", APIToken{}, err
	}
	id := hex.EncodeToString(idBytes)
	token := "rush_" + id + "." + base64.RawURLEncoding.EncodeToString(secretBytes)
	hash := apiTokenDigest(token)
	var expiresCopy *time.Time
	if expires != nil {
		value := *expires
		expiresCopy = &value
	}
	record := APIToken{ID: id, Name: name, Scopes: append([]string(nil), scopes...), Permission: permission, Created: time.Now().UTC(), Expires: expiresCopy, Hash: hex.EncodeToString(hash[:])}
	if _, ok := parseAPIToken(token); !ok {
		return "", APIToken{}, errInvalidAPIToken
	}
	err := s.mutate(func(doc *apiTokenDocument) error {
		for _, old := range doc.Tokens {
			if old.ID == id {
				return errors.New("token id collision")
			}
		}
		doc.Tokens = append(doc.Tokens, record)
		return nil
	})
	if err != nil {
		return "", APIToken{}, err
	}
	return token, record.clone(), nil
}

// ListAPITokens returns fresh records ordered by creation time and never exposes credentials.
func (s *APITokenStore) ListAPITokens() ([]APIToken, error) {
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	tokens := make([]APIToken, len(doc.Tokens))
	for i := range doc.Tokens {
		tokens[i] = doc.Tokens[i].clone()
	}
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].Created.Equal(tokens[j].Created) {
			return tokens[i].ID < tokens[j].ID
		}
		return tokens[i].Created.Before(tokens[j].Created)
	})
	return tokens, nil
}

// ShowAPIToken returns a record by id, without its plaintext credential.
func (s *APITokenStore) ShowAPIToken(id string) (APIToken, error) {
	doc, err := s.read()
	if err != nil {
		return APIToken{}, err
	}
	for _, token := range doc.Tokens {
		if token.ID == id {
			return token.clone(), nil
		}
	}
	return APIToken{}, ErrAPITokenNotFound
}

// RevokeAPIToken makes revocation durable immediately, including to other store instances.
func (s *APITokenStore) RevokeAPIToken(id string) error {
	return s.mutate(func(doc *apiTokenDocument) error {
		for i := range doc.Tokens {
			if doc.Tokens[i].ID == id {
				if doc.Tokens[i].RevokedAt == nil {
					now := time.Now().UTC()
					doc.Tokens[i].RevokedAt = &now
				}
				return nil
			}
		}
		return ErrAPITokenNotFound
	})
}

// VerifyAPIToken performs a fresh disk read for every verification and compares the full-token hash in constant time.
func (s *APITokenStore) VerifyAPIToken(plain string, now time.Time) (APIToken, bool, error) {
	id, ok := parseAPIToken(plain)
	if !ok {
		return APIToken{}, false, errInvalidAPIToken
	}

	doc, err := s.read()
	if err != nil {
		return APIToken{}, false, err
	}
	got := apiTokenDigest(plain)
	var record *APIToken
	for i := range doc.Tokens {
		if doc.Tokens[i].ID == id {
			record = &doc.Tokens[i]
			break
		}
	}
	if record == nil {
		s.bootstrapMu.Lock()
		if s.bootstrap != nil && s.bootstrap.ID == id {
			bootstrap := *s.bootstrap
			record = &bootstrap
		}
		s.bootstrapMu.Unlock()
	}
	want := make([]byte, sha256.Size) // Fixed dummy digest for unknown ids.
	validRecord := record != nil
	if validRecord {
		decoded, decodeErr := hex.DecodeString(record.Hash)
		if decodeErr != nil || len(decoded) != sha256.Size {
			return APIToken{}, false, errInvalidAPIToken
		}
		want = decoded
	}
	compare := apiTokenCompareHook
	if compare == nil {
		compare = subtle.ConstantTimeCompare
	}
	matched := compare(want, got[:]) == 1
	if !validRecord || !matched || record.RevokedAt != nil || (record.Expires != nil && !now.Before(*record.Expires)) {
		return APIToken{}, false, errInvalidAPIToken
	}
	s.usedMu.Lock()
	if s.lastUsed == nil {
		s.lastUsed = make(map[string]time.Time)
	}
	if now.After(s.lastUsed[id]) {
		s.lastUsed[id] = now
	}
	s.usedMu.Unlock()
	return record.clone(), true, nil
}

func parseAPIToken(plain string) (id string, ok bool) {
	if !strings.HasPrefix(plain, "rush_") {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(plain, "rush_"), ".")
	if len(parts) != 2 || !apiTokenIDPattern.MatchString(parts[0]) || !apiTokenSecretPattern.MatchString(parts[1]) {
		return "", false
	}
	return parts[0], true
}

func (s *APITokenStore) read() (apiTokenDocument, error) {
	if s.Path == "" {
		return apiTokenDocument{}, errors.New("empty API token store path")
	}
	path, err := s.selectedPath()
	if err != nil {
		return apiTokenDocument{}, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return apiTokenDocument{Version: apiTokenStoreVersion, Tokens: []APIToken{}}, nil
		}
		return apiTokenDocument{}, statErr
	}
	if !info.Mode().IsRegular() {
		return apiTokenDocument{}, errors.New("invalid API token store file")
	}
	owner, ownerKnown := configFileOwner(info)
	if !ownerKnown || owner != homeConfigOwner() {
		return apiTokenDocument{}, errConfigOwnerMismatch
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0) {
		return apiTokenDocument{}, errors.New("invalid API token store permissions")
	}
	identity := configFileIdentityOf(info)
	if runtime.GOOS == "windows" {
		identity, statErr = configFileIdentityAtPath(path)
		if statErr != nil {
			return apiTokenDocument{}, statErr
		}
	}
	fingerprint := tokenFingerprint{size: info.Size(), modified: info.ModTime(), identity: fmt.Sprintf("%v", identity)}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheValid && s.cachedFingerprint.size == fingerprint.size && s.cachedFingerprint.modified.Equal(fingerprint.modified) && s.cachedFingerprint.identity == fingerprint.identity {
		// Cached documents are immutable after publication; callers must not mutate their backing data.
		return s.cached, nil
	}

	if s.statHook != nil {
		s.statHook()
	}
	if s.readHook != nil {
		s.readHook()
	}
	data, stable, err := readStableConfigFileOwned(path, homeConfigOwner(), true)
	if err != nil {
		return apiTokenDocument{}, err
	}
	fingerprint.size = stable.size
	fingerprint.modified = time.Unix(0, stable.modTime)
	fingerprint.digest = stable.digest
	fingerprint.identity = fmt.Sprintf("%v", stable.identity)
	var doc apiTokenDocument
	document, err := decodeSettingsDocument(path, data, stable)
	if err != nil {
		return apiTokenDocument{}, fmt.Errorf("invalid API token store: %w", err)
	}
	if err := json.Unmarshal(document.jsonBytes, &doc); err != nil {
		return apiTokenDocument{}, fmt.Errorf("invalid API token store: %w", err)
	}
	if doc.Version != apiTokenStoreVersion || doc.Tokens == nil {
		return apiTokenDocument{}, errors.New("invalid API token store version or records")
	}
	for _, t := range doc.Tokens {
		if !apiTokenIDPattern.MatchString(t.ID) || strings.TrimSpace(t.Name) == "" || t.Hash == "" {
			return apiTokenDocument{}, errors.New("invalid API token record")
		}
		digest, err := hex.DecodeString(t.Hash)
		if err != nil || len(digest) != sha256.Size {
			return apiTokenDocument{}, errors.New("invalid API token digest")
		}
		if t.Permission != "ask" && t.Permission != "readonly" && t.Permission != "auto" {
			return apiTokenDocument{}, errors.New("invalid API token permission")
		}
		if len(t.Scopes) == 0 {
			return apiTokenDocument{}, errors.New("invalid API token scopes")
		}
		admin := false
		for _, scope := range t.Scopes {
			if scope != "read" && scope != "run" && scope != "config" && scope != "admin" {
				return apiTokenDocument{}, errors.New("invalid API token scope")
			}
			if scope == "admin" {
				admin = true
			}
		}
		if t.Permission == "auto" && !admin {
			return apiTokenDocument{}, errors.New("auto permission requires admin scope")
		}
	}
	s.cached, s.cachedFingerprint, s.cacheValid = doc, fingerprint, true
	return doc, nil
}

func (s *APITokenStore) selectedPath() (string, error) {
	jsonPath := s.Path
	if filepath.Ext(strings.ToLower(jsonPath)) == ".ktav" {
		jsonPath = strings.TrimSuffix(jsonPath, filepath.Ext(jsonPath)) + ".json"
	}
	ktavPath := strings.TrimSuffix(jsonPath, filepath.Ext(jsonPath)) + ".ktav"
	if _, err := os.Stat(ktavPath); err == nil {
		return ktavPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if newConfigFileIsKtav() {
		return ktavPath, nil
	}
	return jsonPath, nil
}

func cloneAPITokenDocument(doc apiTokenDocument) apiTokenDocument {
	copy := apiTokenDocument{Version: doc.Version, Tokens: make([]APIToken, len(doc.Tokens))}
	for i := range doc.Tokens {
		copy.Tokens[i] = doc.Tokens[i].clone()
	}
	return copy
}

func (s *APITokenStore) mutate(fn func(*apiTokenDocument) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.selectedPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lockPath := configWriteLockAnchor(path)
	chosenPath := path
	if apiTokenBeforeLockHook != nil {
		apiTokenBeforeLockHook(s)
	}
	lock, err := filelock.AcquireFileLockContext(ctx, filepath.Clean(lockPath)+".lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	path, err = s.selectedPath()
	if err != nil {
		return err
	}
	if filepath.Clean(path) != filepath.Clean(chosenPath) {
		return errors.New("API token store path changed while acquiring lock")
	}
	doc, err := s.read()
	if err != nil {
		return err
	}
	// Mutations operate on private slices and pointed-to fields so a failed write
	// cannot change the cached snapshot that concurrent readers may be using.
	doc = cloneAPITokenDocument(doc)
	if s.mutateHook != nil {
		s.mutateHook()
	}
	if err := fn(&doc); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	format := settingsDocumentFormatOf(path)
	encoded, err := encodeSettingsDocument(data, format)
	if err != nil {
		return err
	}
	return apiTokenWriteFile(path, encoded, 0o600)
}

// APITokenScopeAllows reports whether have grants need. Scope hierarchy is
// read < run < config < admin; unknown values never grant access.
func APITokenScopeAllows(have []string, need string) bool {
	levels := map[string]int{"read": 1, "run": 2, "config": 3, "admin": 4}
	needed, ok := levels[need]
	if !ok {
		return false
	}
	for _, scope := range have {
		if level, ok := levels[scope]; ok && level >= needed {
			return true
		}
	}
	return false
}

// ConsumeServeBootstrapToken reads the optional bootstrap token at most once per store.
// Both environment variables are removed before validation; the first result,
// including an error, is returned on every subsequent call.
func ConsumeServeBootstrapToken(store *APITokenStore, getenv func(string) string, unsetenv func(string) error, now time.Time) (APIToken, bool, error) {
	store.bootstrapMu.Lock()
	defer store.bootstrapMu.Unlock()
	if store.bootstrapConsumed {
		return store.bootstrapResult.clone(), store.bootstrapOK, store.bootstrapErr
	}
	store.bootstrapConsumed = true
	store.bootstrapResult, store.bootstrapOK, store.bootstrapErr = consumeServeBootstrapToken(store, getenv, unsetenv, now)
	return store.bootstrapResult.clone(), store.bootstrapOK, store.bootstrapErr
}

// ConsumeServeBootstrapTokenFromEnv consumes the production serve bootstrap environment.
func ConsumeServeBootstrapTokenFromEnv(store *APITokenStore, now time.Time) (APIToken, bool, error) {
	return ConsumeServeBootstrapToken(store, os.Getenv, os.Unsetenv, now)
}

func consumeServeBootstrapToken(store *APITokenStore, getenv func(string) string, unsetenv func(string) error, now time.Time) (APIToken, bool, error) {
	value := getenv("RUSH_SERVE_TOKEN_SHA256")
	scope := getenv("RUSH_SERVE_TOKEN_SCOPE")
	if err := unsetenv("RUSH_SERVE_TOKEN_SHA256"); err != nil {
		return APIToken{}, false, err
	}
	if err := unsetenv("RUSH_SERVE_TOKEN_SCOPE"); err != nil {
		return APIToken{}, false, err
	}
	if value == "" {
		return APIToken{}, false, nil
	}
	if scope == "" {
		scope = "run"
	}
	if scope != "read" && scope != "run" && scope != "config" {
		return APIToken{}, false, errors.New("invalid RUSH_SERVE_TOKEN_SCOPE")
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 || !apiTokenIDPattern.MatchString(parts[0]) || len(parts[1]) != sha256.Size*2 || strings.ToLower(parts[1]) != parts[1] {
		return APIToken{}, false, errors.New("invalid RUSH_SERVE_TOKEN_SHA256")
	}
	want, err := hex.DecodeString(parts[1])
	if err != nil {
		return APIToken{}, false, errors.New("invalid RUSH_SERVE_TOKEN_SHA256")
	}
	bootstrap := APIToken{ID: parts[0], Name: "bootstrap", Scopes: []string{scope}, Permission: "ask", Created: now.UTC(), Hash: hex.EncodeToString(want)}
	doc, err := store.read()
	if err != nil {
		return APIToken{}, false, err
	}
	for _, saved := range doc.Tokens {
		if saved.ID == bootstrap.ID {
			return APIToken{}, false, errors.New("bootstrap token id collides with stored token")
		}
	}
	store.bootstrap = func() *APIToken { copy := bootstrap.clone(); return &copy }()
	return bootstrap.clone(), true, nil
}

var apiTokenClock = time.Now

func (s *APITokenStore) Touch(id string) {
	s.usedMu.Lock()
	defer s.usedMu.Unlock()
	if s.lastUsed == nil {
		s.lastUsed = make(map[string]time.Time)
	}
	now := apiTokenClock().UTC()
	if now.After(s.lastUsed[id]) {
		s.lastUsed[id] = now
	}
}

func (s *APITokenStore) RecordAPITokenUse(id string, now time.Time) error {
	s.usedMu.Lock()
	if s.lastUsed == nil {
		s.lastUsed = make(map[string]time.Time)
	}
	if now.After(s.lastUsed[id]) {
		s.lastUsed[id] = now
	}
	s.usedMu.Unlock()
	return nil
}

// FlushAPITokenUsage persists the in-memory last-use timestamps no more than
// once per minute. It uses a state-specific lock and merges monotonically.
func (s *APITokenStore) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	now := apiTokenClock().UTC()
	s.usedMu.Lock()
	if !s.lastFlush.IsZero() && now.Sub(s.lastFlush) < time.Minute {
		s.usedMu.Unlock()
		return nil
	}
	snapshot := make(map[string]time.Time, len(s.lastUsed))
	for id, used := range s.lastUsed {
		snapshot[id] = used
	}
	s.usedMu.Unlock()

	if err := s.flushLastUsedSnapshot(now, snapshot); err != nil {
		return err
	}
	s.usedMu.Lock()
	s.lastFlush = now
	s.usedMu.Unlock()
	return nil
}

func (s *APITokenStore) flushLastUsedSnapshot(now time.Time, lastUsed map[string]time.Time) error {
	statePath := filepath.Join(filepath.Dir(s.Path), "api-tokens.state")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lock, err := filelock.AcquireFileLockContext(ctx, statePath+".lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	merged := map[string]time.Time{}
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &merged); err != nil || merged == nil {
			return errors.New("invalid API token state file")
		}
		for id, used := range merged {
			if !apiTokenIDPattern.MatchString(id) || used.IsZero() {
				return errors.New("invalid API token state file")
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for tokenID, used := range lastUsed {
		if used.After(merged[tokenID]) {
			merged[tokenID] = used
		}
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return atomicWriteFile(statePath, data, 0o600)
}

func apiTokenDigest(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

func APITokenHash(token string) string {
	sum := apiTokenDigest(token)
	return hex.EncodeToString(sum[:])
}
