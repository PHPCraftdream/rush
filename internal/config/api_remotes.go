package config

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

var (
	apiRemoteNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	apiRemoteTokenPattern = regexp.MustCompile(`^rush_[0-9a-f]{8}\.[A-Za-z0-9_-]{43}$`)
	apiRemoteEnvPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	apiRemoteMutationMu   sync.Mutex
)

// APIRemote configures one global external API endpoint and its credential source.
type APIRemote struct {
	URL            string `json:"url,omitempty"`
	TokenEnv       string `json:"token_env,omitempty"`
	TokenFile      string `json:"token_file,omitempty"`
	CA             string `json:"ca,omitempty"`
	Pin            string `json:"pin,omitempty"`
	ClientCert     string `json:"client_cert,omitempty"`
	ClientKey      string `json:"client_key,omitempty"`
	AllowCleartext bool   `json:"allow_cleartext,omitempty"`
}

// LooksLikeAPIToken detects token-shaped input without disclosing its contents.
func LooksLikeAPIToken(s string) bool { return apiRemoteTokenPattern.MatchString(s) }

func validateAPIRemoteName(name string) error {
	if LooksLikeAPIToken(name) {
		return errors.New("token-looking name refused; rotate the token")
	}
	if !apiRemoteNamePattern.MatchString(name) {
		return errors.New("invalid API remote name")
	}
	return nil
}

func ValidateAPIRemote(remote APIRemote) error {
	if LooksLikeAPIToken(remote.TokenEnv) {
		return errors.New("token-looking token_env refused; rotate the token")
	}
	if (remote.TokenEnv == "") == (remote.TokenFile == "") {
		return errors.New("exactly one credential source is required")
	}
	if remote.TokenEnv != "" && (!apiRemoteEnvPattern.MatchString(remote.TokenEnv) || remote.TokenEnv == "RUSH_TOKENS") {
		return errors.New("invalid token_env variable name")
	}
	parsed, err := url.Parse(remote.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("invalid API remote URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return errors.New("API remote URL must use https")
	}
	if parsed.Scheme == "http" && !remote.AllowCleartext && !isLoopbackHost(parsed.Hostname()) {
		return errors.New("cleartext HTTP requires allow_cleartext or a loopback host")
	}
	if remote.Pin != "" && !validAPIRemotePin(remote.Pin) {
		return errors.New("invalid API remote pin")
	}
	if (remote.ClientCert == "") != (remote.ClientKey == "") {
		return errors.New("client_cert and client_key must be specified together")
	}
	for _, path := range []string{remote.TokenFile, remote.CA, remote.ClientCert, remote.ClientKey} {
		if path != "" && !filepath.IsAbs(path) && path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~\\") {
			return errors.New("API remote paths must be absolute or home-relative")
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validAPIRemotePin(pin string) bool {
	if strings.HasPrefix(pin, "sha256//") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pin, "sha256//"))
		return err == nil && len(decoded) == 32
	}
	if strings.HasPrefix(pin, "cert-sha256:") {
		decoded, err := hex.DecodeString(strings.TrimPrefix(pin, "cert-sha256:"))
		return err == nil && len(decoded) == 32 && len(strings.TrimPrefix(pin, "cert-sha256:")) == 64
	}
	return false
}

func globalAPIRemoteStore() (*ConfigStore, error) { return Load("", "", false) }

func ListAPIRemotes() (map[string]APIRemote, string, error) {
	data, err := os.ReadFile(GlobalConfigData())
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	profiles := make(map[string]APIRemote)
	result := gjson.GetBytes(data, "api.remotes")
	if result.Exists() {
		if err := json.Unmarshal([]byte(result.Raw), &profiles); err != nil {
			return nil, "", err
		}
		if profiles == nil {
			profiles = make(map[string]APIRemote)
		}
	}
	return profiles, gjson.GetBytes(data, "api.default_remote").String(), nil
}

func GetAPIRemote(name string) (APIRemote, error) {
	if err := validateAPIRemoteName(name); err != nil {
		return APIRemote{}, err
	}
	profiles, _, err := ListAPIRemotes()
	if err != nil {
		return APIRemote{}, err
	}
	remote, ok := profiles[name]
	if !ok {
		return APIRemote{}, fmt.Errorf("API remote %q not found", name)
	}
	return remote, nil
}

func SetAPIRemote(name string, remote APIRemote, replace bool) error {
	if err := validateAPIRemoteName(name); err != nil {
		return err
	}
	if err := ValidateAPIRemote(remote); err != nil {
		return err
	}
	apiRemoteMutationMu.Lock()
	defer apiRemoteMutationMu.Unlock()
	profiles, _, err := ListAPIRemotes()
	if err != nil {
		return err
	}
	if _, ok := profiles[name]; ok && !replace {
		return errors.New("API remote exists; explicit replace required")
	}
	store, err := globalAPIRemoteStore()
	if err != nil {
		return err
	}
	if err := store.SetConfigField(ScopeGlobal, "api.remotes."+name, remote); err != nil {
		return err
	}
	return nil
}

func RemoveAPIRemote(name string) error {
	if err := validateAPIRemoteName(name); err != nil {
		return err
	}
	apiRemoteMutationMu.Lock()
	defer apiRemoteMutationMu.Unlock()
	store, err := globalAPIRemoteStore()
	if err != nil {
		return err
	}
	_, defaultName, err := ListAPIRemotes()
	if err != nil {
		return err
	}
	if err := store.RemoveConfigField(ScopeGlobal, "api.remotes."+name); err != nil {
		return err
	}
	if defaultName == name {
		if err := store.RemoveConfigField(ScopeGlobal, "api.default_remote"); err != nil {
			return err
		}
	}
	path := filepath.Join(filepath.Dir(GlobalConfigData()), "remotes", name+".token")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func SetDefaultAPIRemote(name string) error {
	apiRemoteMutationMu.Lock()
	defer apiRemoteMutationMu.Unlock()
	if _, err := GetAPIRemote(name); err != nil {
		return err
	}
	store, err := globalAPIRemoteStore()
	if err != nil {
		return err
	}
	return store.SetConfigField(ScopeGlobal, "api.default_remote", name)
}

func ResolveAPIRemote(selected string, getenv func(string) string) (string, APIRemote, error) {
	profiles, defaultName, err := ListAPIRemotes()
	if err != nil {
		return "", APIRemote{}, err
	}
	name := selected
	if name == "" && getenv != nil {
		name = getenv("RUSH_REMOTE")
	}
	if name == "" {
		name = defaultName
	}
	if name == "" {
		return "", APIRemote{}, errors.New("no remote selected")
	}
	if LooksLikeAPIToken(name) {
		return "", APIRemote{}, errors.New("token-looking remote name refused; rotate the token")
	}
	remote, ok := profiles[name]
	if !ok {
		return "", APIRemote{}, fmt.Errorf("API remote %q not found", name)
	}
	return name, remote, nil
}

func ReadAPIRemoteToken(remote APIRemote, getenv func(string) string) (string, error) {
	if LooksLikeAPIToken(remote.TokenEnv) {
		return "", errors.New("token-looking token_env refused; rotate the token")
	}
	if remote.TokenEnv != "" {
		if getenv == nil || getenv(remote.TokenEnv) == "" {
			return "", fmt.Errorf("environment variable %s is empty", remote.TokenEnv)
		}
		return getenv(remote.TokenEnv), nil
	}
	if remote.TokenFile == "" {
		return "", errors.New("remote has no credential source")
	}
	path, err := expandRemotePath(remote.TokenFile)
	if err != nil {
		return "", errors.New("invalid token file path")
	}
	if runtime.GOOS == "windows" {
		home, homeErr := os.UserHomeDir()
		resolved, resolveErr := filepath.EvalSymlinks(path)
		homeResolved, homeResolveErr := filepath.EvalSymlinks(home)
		if homeErr != nil || resolveErr != nil || homeResolveErr != nil || filepath.Clean(resolved) != filepath.Clean(path) || filepath.Clean(homeResolved) != filepath.Clean(home) || !pathWithin(homeResolved, resolved) {
			return "", errors.New("token file must be under the user profile without symlinks")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", errors.New("token file unavailable")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("token file permissions are too broad")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("token file unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return "", errors.New("token file exceeds 4 KiB")
	}
	token := strings.TrimSuffix(string(data), "\n")
	if token == "" {
		return "", errors.New("token file is empty")
	}
	return token, nil
}

func expandRemotePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	return filepath.Clean(path), nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func WriteAPIRemoteTokenFile(name string, token []byte) (string, error) {
	if err := validateAPIRemoteName(name); err != nil {
		return "", err
	}
	if !LooksLikeAPIToken(string(token)) {
		return "", errors.New("token does not match API token format")
	}
	// Windows does not provide Unix chmod semantics; the user profile ACL protects this file.
	dir := filepath.Join(filepath.Dir(GlobalConfigData()), "remotes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	path := filepath.Join(dir, name+".token")
	if err := atomicWriteFile(path, token, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
