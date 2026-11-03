package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// APITokenBinding permanently narrows the operations a token may perform.
type APITokenBinding struct {
	Kind  string                `json:"kind"`
	Pin   *APITokenPinBinding   `json:"pin,omitempty"`
	Batch *APITokenBatchBinding `json:"batch,omitempty"`
}

// APITokenPinBinding identifies one session and its canonical workspace.
type APITokenPinBinding struct {
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
	Create    bool   `json:"create"`
}

// APITokenBatchBinding describes permitted batch intake.
type APITokenBatchBinding struct {
	Tools       []string `json:"tools"`
	MaxRoots    int      `json:"max_roots"`
	MaxParallel int      `json:"max_parallel"`
}

// CanonicalAPIWorkspace returns an absolute, symlink-resolved workspace path.
// Windows paths are case-folded so equivalent spellings compare consistently.
func CanonicalAPIWorkspace(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("workspace path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		resolved = strings.ToLower(resolved)
	}
	return filepath.Clean(resolved), nil
}

func (b APITokenBinding) Validate(scopes []string) error {
	kind := b.Kind
	if kind == "" {
		kind = "none"
	}
	admin := false
	for _, scope := range scopes {
		admin = admin || scope == "admin"
	}
	if kind != "none" && kind != "session" && kind != "batch" {
		return errors.New("invalid API token binding kind")
	}
	if kind != "none" && admin {
		return errors.New("API token binding cannot be combined with admin scope")
	}
	if kind == "session" {
		if b.Pin == nil || b.Batch != nil || b.Pin.Session == "" || b.Pin.Workspace == "" {
			return errors.New("session binding requires a session pin and workspace")
		}
	} else if b.Pin != nil {
		return errors.New("pin is only valid for a session binding")
	}
	if kind == "batch" {
		if b.Batch == nil || b.Pin != nil || b.Batch.MaxRoots < 0 || b.Batch.MaxParallel < 0 {
			return errors.New("batch binding requires valid batch limits")
		}
	} else if b.Batch != nil {
		return errors.New("batch is only valid for a batch binding")
	}
	return nil
}

func (b APITokenBinding) Normalized() APITokenBinding {
	if b.Kind == "" {
		b.Kind = "none"
	}
	return b
}

func (b APITokenBinding) Clone() APITokenBinding {
	out := b
	if b.Pin != nil {
		pin := *b.Pin
		out.Pin = &pin
	}
	if b.Batch != nil {
		batch := *b.Batch
		batch.Tools = append([]string(nil), b.Batch.Tools...)
		out.Batch = &batch
	}
	return out
}
