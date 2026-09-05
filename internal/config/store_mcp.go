package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// PersistMCPConfig atomically upserts one literal MCP server key in a config
// file. The server name is a JSON object key, not a sjson path, so dots,
// backslashes, wildcards, and every other supported key character are kept
// verbatim.
func (s *ConfigStore) PersistMCPConfig(scope Scope, name string, value MCPConfig) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to encode MCP server %q: %w", name, err)
	}
	return s.persistMCPRaw(scope, func(servers map[string]json.RawMessage) error {
		servers[name] = raw
		return nil
	})
}

// PersistRemoveMCPConfig atomically removes one literal MCP server key from a
// config file.
func (s *ConfigStore) PersistRemoveMCPConfig(scope Scope, name string) error {
	return s.persistMCPRaw(scope, func(servers map[string]json.RawMessage) error {
		delete(servers, name)
		return nil
	})
}

// PersistMCPDisabledOverride atomically writes a disabled override for one
// literal MCP server key. This is used for servers supplied by .mcp.json.
func (s *ConfigStore) PersistMCPDisabledOverride(scope Scope, name string, disabled bool) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.persistMCPRawAt(path, func(servers map[string]json.RawMessage) error {
		entry := make(map[string]json.RawMessage)
		if raw := servers[name]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &entry); err != nil {
				return fmt.Errorf("failed to parse MCP server %q: %w", name, err)
			}
		}
		if entry == nil {
			entry = make(map[string]json.RawMessage)
		}
		disabledJSON, err := json.Marshal(disabled)
		if err != nil {
			return fmt.Errorf("failed to encode MCP disabled state: %w", err)
		}
		entry["disabled"] = disabledJSON
		raw, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("failed to encode MCP server %q: %w", name, err)
		}
		servers[name] = raw
		return nil
	})
}

// PersistReplaceMCP atomically removes oldName and sets newName in the
// global MCP map, then publishes the matching copy-on-write snapshot. Disk is
// committed before the snapshot changes, and the operation has no fallible
// work after publication.
func (s *ConfigStore) PersistReplaceMCP(oldName, newName string, value MCPConfig) error {
	path, err := s.configPath(ScopeGlobal)
	if err != nil {
		return err
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if err := s.persistMCPRawAt(path, func(servers map[string]json.RawMessage) error {
		delete(servers, oldName)
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("failed to encode MCP server %q: %w", newName, err)
		}
		servers[newName] = raw
		return nil
	}); err != nil {
		return err
	}

	cur := s.loadSnapshot()
	next := cur.clone()
	if cur.config != nil {
		cfgCopy := *cur.config
		cfgCopy.MCP = maps.Clone(cur.config.MCP)
		if cfgCopy.MCP == nil {
			cfgCopy.MCP = make(MCPs)
		}
		delete(cfgCopy.MCP, oldName)
		cfgCopy.MCP[newName] = cloneMCPConfig(value)
		next.config = &cfgCopy
	}
	s.publishLocked(next)
	return nil
}

func (s *ConfigStore) persistMCPRaw(scope Scope, mutate func(map[string]json.RawMessage) error) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.persistMCPRawAt(path, mutate)
}

func (s *ConfigStore) persistMCPRawAt(path string, mutate func(map[string]json.RawMessage) error) error {
	return s.withConfigWriteLock(path, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				data = []byte("{}")
			} else {
				return fmt.Errorf("failed to read config file: %w", err)
			}
		}

		var root map[string]json.RawMessage
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("failed to parse config file: %w", err)
		}
		if root == nil {
			root = make(map[string]json.RawMessage)
		}
		servers := make(map[string]json.RawMessage)
		if raw := root["mcp"]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &servers); err != nil {
				return fmt.Errorf("failed to parse MCP config: %w", err)
			}
		}
		if servers == nil {
			servers = make(map[string]json.RawMessage)
		}
		if err := mutate(servers); err != nil {
			return err
		}
		raw, err := json.Marshal(servers)
		if err != nil {
			return fmt.Errorf("failed to encode MCP config: %w", err)
		}
		root["mcp"] = raw
		newValue, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to encode config file: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		if err := atomicWriteFile(path, append(newValue, '\n'), 0o600); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
		return nil
	})
}

// mcpLiteralField reports the exact server name and optional field encoded by
// a legacy dotted config key. It is intentionally small: callers use the
// whole-map persistence APIs above for writes, while this parser helps reads
// remain compatible with existing callers.
func mcpLiteralField(key string) (name, field string, ok bool) {
	if len(key) < len("mcp.") || key[:len("mcp.")] != "mcp." {
		return "", "", false
	}
	rest := key[len("mcp."):]
	for _, candidate := range []string{"disabled", "command", "args", "env", "url", "headers", "timeout", "type", "disabled_tools", "enabled_tools", "enabled_in_cli"} {
		suffix := "." + candidate
		if len(rest) > len(suffix) && rest[len(rest)-len(suffix):] == suffix {
			return rest[:len(rest)-len(suffix)], candidate, true
		}
	}
	return rest, "", rest != ""
}

func readLiteralMCPField(data []byte, name, field string) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(root["mcp"], &servers) != nil {
		return false
	}
	raw, exists := servers[name]
	if !exists {
		return false
	}
	if field == "" {
		return true
	}
	var server map[string]json.RawMessage
	if json.Unmarshal(raw, &server) != nil {
		return false
	}
	_, exists = server[field]
	return exists
}

func (s *ConfigStore) setMCPFields(scope Scope, fields map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return fmt.Errorf("%v: %w", fields, err)
	}
	s.publishMu.Lock()
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	err = s.persistMCPRawAt(path, func(servers map[string]json.RawMessage) error {
		for _, key := range keys {
			value := fields[key]
			name, field, ok := mcpLiteralField(key)
			if !ok {
				return fmt.Errorf("invalid MCP config field %q", key)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("failed to encode config field %s: %w", key, err)
			}
			if field == "" {
				servers[name] = raw
				continue
			}
			entry := make(map[string]json.RawMessage)
			if existing := servers[name]; len(existing) > 0 {
				if err := json.Unmarshal(existing, &entry); err != nil {
					return fmt.Errorf("failed to parse MCP server %q: %w", name, err)
				}
			}
			if entry == nil {
				entry = make(map[string]json.RawMessage)
			}
			entry[field] = raw
			updated, err := json.Marshal(entry)
			if err != nil {
				return fmt.Errorf("failed to encode MCP server %q: %w", name, err)
			}
			servers[name] = updated
		}
		return nil
	})
	s.publishMu.Unlock()
	if err != nil {
		return err
	}
	if err := s.autoReload(context.Background()); err != nil {
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}
	return nil
}

func (s *ConfigStore) removeMCPField(scope Scope, key string) error {
	path, err := s.configPath(scope)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat config file: %w", err)
	}
	name, field, ok := mcpLiteralField(key)
	if !ok {
		return fmt.Errorf("invalid MCP config field %q", key)
	}
	s.publishMu.Lock()
	err = s.persistMCPRawAt(path, func(servers map[string]json.RawMessage) error {
		if field == "" {
			delete(servers, name)
			return nil
		}
		raw, exists := servers[name]
		if !exists {
			return nil
		}
		entry := make(map[string]json.RawMessage)
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("failed to parse MCP server %q: %w", name, err)
		}
		if entry == nil {
			entry = make(map[string]json.RawMessage)
		}
		delete(entry, field)
		updated, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("failed to encode MCP server %q: %w", name, err)
		}
		servers[name] = updated
		return nil
	})
	s.publishMu.Unlock()
	if err != nil {
		return err
	}
	if err := s.autoReload(context.Background()); err != nil {
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}
	return nil
}
