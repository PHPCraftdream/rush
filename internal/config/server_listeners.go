package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/qjebbs/go-jsons"
)

var (
	ErrListenerConfigRead   = errors.New("could not read listener config")
	ErrListenerConfigDecode = errors.New("decode global listener config")
)

// configuration-layer data and does not depend on internal/server.
type ServerListener struct {
	Addr             string               `json:"addr"`
	Surfaces         []string             `json:"surfaces,omitempty"`
	TLS              ServerListenerTLS    `json:"tls,omitempty"`
	AllowCleartext   bool                 `json:"allow_cleartext,omitempty"`
	AllowAdmin       bool                 `json:"allow_admin,omitempty"`
	AllowAutoApprove bool                 `json:"allow_auto_approve,omitempty"`
	AllowedOrigins   []string             `json:"allowed_origins,omitempty"`
	Tokens           []string             `json:"tokens,omitempty"`
	Route            string               `json:"route,omitempty"`
	Pins             map[string]string    `json:"pins,omitempty"`
	Limits           ServerListenerLimits `json:"limits,omitempty"`
}

// ServerListenerTLS contains the configured TLS file paths and minimum version.
type ServerListenerTLS struct {
	Cert       string `json:"cert,omitempty"`
	Key        string `json:"key,omitempty"`
	ClientCA   string `json:"client_ca,omitempty"`
	MinVersion string `json:"min_version,omitempty"`
}

// ServerListenerLimits contains the configured listener resource limits.
type ServerListenerLimits struct {
	RPS              float64 `json:"rps,omitempty"`
	Burst            int     `json:"burst,omitempty"`
	MaxConnsPerToken int     `json:"max_conns_per_token,omitempty"`
	MaxBody          int64   `json:"max_body,omitempty"`
}

// ServerListenerFlags are explicitly supplied command-line values. Nil fields
// leave the first configured listener unchanged; non-nil values override it,
// including false and empty values.
type ServerListenerFlags struct {
	Addr             *string
	AllowCleartext   *bool
	AllowAdmin       *bool
	AllowAutoApprove *bool
	TLSCert          *string
	TLSKey           *string
	TLSClientCA      *string
	TLSMinVersion    *string
}

func (l *ServerListenerLimits) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	maxBody, ok := raw["max_body"]
	delete(raw, "max_body")
	other, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	type limitsAlias ServerListenerLimits
	var decoded limitsAlias
	if err := json.Unmarshal(other, &decoded); err != nil {
		return err
	}
	*l = ServerListenerLimits(decoded)
	if !ok {
		return nil
	}
	var n int64
	if err := json.Unmarshal(maxBody, &n); err == nil {
		l.MaxBody = n
		return nil
	}
	var text string
	if err := json.Unmarshal(maxBody, &text); err != nil {
		return fmt.Errorf("max_body must be a byte count or size: %w", err)
	}
	text = strings.TrimSpace(strings.ToLower(text))
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		factor int64
	}{
		{"mib", 1024 * 1024},
		{"kib", 1024},
		{"gib", 1024 * 1024 * 1024},
		{"mb", 1000 * 1000},
		{"kb", 1000},
		{"gb", 1000 * 1000 * 1000},
		{"b", 1},
	} {
		if strings.HasSuffix(text, unit.suffix) {
			text = strings.TrimSpace(strings.TrimSuffix(text, unit.suffix))
			multiplier = unit.factor
			break
		}
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed < 0 || parsed > int64(^uint64(0)>>1)/multiplier {
		return fmt.Errorf("invalid max_body size %q", text)
	}
	l.MaxBody = parsed * multiplier
	return nil
}

// GlobalServerListeners reads only the system and global logical config slots.
// Project/workspace config is deliberately not an input. The returned values
// are a startup snapshot; callers should retain them rather than reload.
func GlobalServerListeners(flags ServerListenerFlags) ([]ServerListener, error) {
	listeners, _, err := GlobalServerListenersWithSources(flags)
	return listeners, err
}

// GlobalServerListenersWithSources returns listeners and per-field provenance
// corresponding by index to each listener. Config sources are evaluated in
// merge order (system, then global); later documents win for fields they set.
func GlobalServerListenersWithSources(flags ServerListenerFlags) ([]ServerListener, []map[string]string, error) {
	paths := []struct {
		path  string
		owner int
	}{
		{systemConfigSlot(), systemConfigOwner()},
		{GlobalConfig(), homeConfigOwner()},
		{GlobalConfigData(), homeConfigOwner()},
	}
	var documents []serverListenerConfigDocument
	for _, candidate := range paths {
		if candidate.path == "" {
			continue
		}
		path := selectConfigSlot(candidate.path)
		data, _, err := readStableConfigFileOwned(path, candidate.owner, true)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: read global listener config %s: %w", ErrListenerConfigRead, path, err)
		}
		data, err = decodeConfigDocument(path, data)
		if err != nil {
			return nil, nil, fmt.Errorf("%w %s: %w", ErrListenerConfigDecode, path, err)
		}
		source := "global"
		if candidate.path == systemConfigSlot() {
			source = "system config"
		} else {
			source = "global config"
		}
		documents = append(documents, serverListenerConfigDocument{data: data, source: source, path: path})
	}
	return globalServerListenersFromDocuments(documents, flags)
}

type serverListenerConfigDocument struct {
	data   []byte
	source string
	path   string
}

// globalServerListenersFromDocuments keeps source tracking on the same merge
// path as values. jsons.Merge's array behavior is mirrored by merging a tree
// whose listener leaf values are replaced with their document source.
func globalServerListenersFromDocuments(documents []serverListenerConfigDocument, flags ServerListenerFlags) ([]ServerListener, []map[string]string, error) {
	if len(documents) == 0 {
		documents = []serverListenerConfigDocument{{data: []byte(`{}`)}}
	}
	values := make([][]byte, 0, len(documents))
	attributions := make([][]byte, 0, len(documents))
	for _, document := range documents {
		values = append(values, document.data)
		annotated, err := annotateServerListenerSources(document.data, document.source)
		if err != nil {
			return nil, nil, fmt.Errorf("%w %s: %w", ErrListenerConfigDecode, document.path, err)
		}
		attributions = append(attributions, annotated)
	}
	merged, err := jsons.Merge(values)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrListenerConfigDecode, err)
	}
	mergedSources, err := jsons.Merge(attributions)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: decode global listener config provenance: %w", ErrListenerConfigDecode, err)
	}
	var root struct {
		Server struct {
			Listeners []ServerListener `json:"listeners"`
		} `json:"server"`
	}
	if err := json.Unmarshal(merged, &root); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrListenerConfigDecode, err)
	}
	var sourceRoot struct {
		Server struct {
			Listeners []map[string]json.RawMessage `json:"listeners"`
		} `json:"server"`
	}
	if err := json.Unmarshal(mergedSources, &sourceRoot); err != nil {
		return nil, nil, fmt.Errorf("%w: decode global listener config provenance: %w", ErrListenerConfigDecode, err)
	}
	listeners := root.Server.Listeners
	sources := make([]map[string]string, len(listeners))
	for i := range listeners {
		sources[i] = make(map[string]string)
		if i < len(sourceRoot.Server.Listeners) {
			for field, raw := range sourceRoot.Server.Listeners[i] {
				if field == "tls" || field == "limits" {
					var nested map[string]string
					if json.Unmarshal(raw, &nested) == nil {
						for name, source := range nested {
							sources[i][field+"."+name] = source
						}
					}
				} else {
					var source string
					if json.Unmarshal(raw, &source) == nil {
						sources[i][field] = source
					}
				}
			}
		}
	}
	if len(listeners) > 0 {
		applyServerListenerFlags(&listeners[0], flags)
		for field, present := range map[string]bool{"addr": flags.Addr != nil, "allow_cleartext": flags.AllowCleartext != nil, "allow_admin": flags.AllowAdmin != nil, "allow_auto_approve": flags.AllowAutoApprove != nil, "tls.cert": flags.TLSCert != nil, "tls.key": flags.TLSKey != nil, "tls.client_ca": flags.TLSClientCA != nil, "tls.min_version": flags.TLSMinVersion != nil} {
			if present {
				sources[0][field] = "flag"
			}
		}
	}
	return listeners, sources, nil
}

func annotateServerListenerSources(data []byte, source string) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	var server map[string]json.RawMessage
	if json.Unmarshal(document["server"], &server) != nil {
		return data, nil
	}
	var listeners []map[string]json.RawMessage
	if json.Unmarshal(server["listeners"], &listeners) != nil {
		return data, nil
	}
	for _, listener := range listeners {
		for key, value := range listener {
			if strings.TrimSpace(string(value)) == "null" {
				continue
			}
			if key == "tls" || key == "limits" {
				var nested map[string]json.RawMessage
				if json.Unmarshal(value, &nested) == nil {
					for name, nestedValue := range nested {
						if strings.TrimSpace(string(nestedValue)) == "null" {
							continue
						}
						nested[name], _ = json.Marshal(source)
					}
					listener[key], _ = json.Marshal(nested)
				}
			} else {
				listener[key], _ = json.Marshal(source)
			}
		}
	}
	server["listeners"], _ = json.Marshal(listeners)
	document["server"], _ = json.Marshal(server)
	return json.Marshal(document)
}

func serverListenerConfiguredFields(raw map[string]json.RawMessage) map[string]bool {
	fields := make(map[string]bool)
	for _, name := range []string{"addr", "surfaces", "allow_cleartext", "allow_admin", "allow_auto_approve", "allowed_origins", "tokens"} {
		if _, ok := raw[name]; ok {
			fields[name] = true
		}
	}
	for _, group := range []string{"tls", "limits"} {
		if data, ok := raw[group]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(data, &nested) == nil {
				for name := range nested {
					fields[group+"."+name] = true
				}
			}
		}
	}
	return fields
}

func applyServerListenerFlags(listener *ServerListener, flags ServerListenerFlags) {
	if flags.Addr != nil {
		listener.Addr = *flags.Addr
	}
	if flags.AllowCleartext != nil {
		listener.AllowCleartext = *flags.AllowCleartext
	}
	if flags.AllowAdmin != nil {
		listener.AllowAdmin = *flags.AllowAdmin
	}
	if flags.AllowAutoApprove != nil {
		listener.AllowAutoApprove = *flags.AllowAutoApprove
	}
	if flags.TLSCert != nil {
		listener.TLS.Cert = *flags.TLSCert
	}
	if flags.TLSKey != nil {
		listener.TLS.Key = *flags.TLSKey
	}
	if flags.TLSClientCA != nil {
		listener.TLS.ClientCA = *flags.TLSClientCA
	}
	if flags.TLSMinVersion != nil {
		listener.TLS.MinVersion = *flags.TLSMinVersion
	}
}
