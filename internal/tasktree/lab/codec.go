package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

func decodeObject(reader io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(reader, MaxInputBytes+1))
	if err != nil {
		return fmt.Errorf("read JSON: %w", err)
	}
	if len(data) > MaxInputBytes {
		return fmt.Errorf("lab input exceeds %d bytes", MaxInputBytes)
	}
	if err := scanJSON(data); err != nil {
		return err
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(value); err != nil {
		return fmt.Errorf("decode JSON fields: %w", err)
	}
	return nil
}

// Token scanning keeps member sets local to each object and bounds nesting before decoding.
func scanJSON(data []byte) error {
	type frame struct {
		object bool
		key    bool
		seen   map[string]bool
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var stack []frame
	complete := false
	for {
		token, err := decoder.Token()
		if err == io.EOF && complete && len(stack) == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scan JSON: %w", err)
		}
		if complete && len(stack) == 0 {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		if delim, ok := token.(json.Delim); ok && (delim == '}' || delim == ']') {
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				complete = true
			}
			continue
		}
		if len(stack) > 0 {
			parent := &stack[len(stack)-1]
			if parent.object && parent.key {
				key, ok := token.(string)
				if !ok {
					return fmt.Errorf("expected object member name")
				}
				if parent.seen[key] {
					return fmt.Errorf("duplicate JSON member %q", key)
				}
				parent.seen[key], parent.key = true, false
				continue
			}
			if parent.object {
				parent.key = true
			}
		}
		if delim, ok := token.(json.Delim); ok {
			if len(stack) >= MaxJSONDepth {
				return fmt.Errorf("lab JSON nesting exceeds %d", MaxJSONDepth)
			}
			f := frame{object: delim == '{', key: delim == '{'}
			if f.object {
				f.seen = make(map[string]bool)
			}
			stack = append(stack, f)
		} else if len(stack) == 0 {
			complete = true
		}
	}
}

func validateBinding(version int, key tasktree.TreeKey, limits tasktree.Limits) error {
	if version != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", version)
	}
	if strings.TrimSpace(string(key)) == "" {
		return fmt.Errorf("tree_key must not be blank")
	}
	return tasktree.ValidateLimits(limits)
}

func validateCheckpoint(checkpoint Checkpoint) error {
	if err := preflightCheckpoint(checkpoint); err != nil {
		return err
	}
	if err := validateBinding(checkpoint.SchemaVersion, checkpoint.TreeKey, checkpoint.Limits); err != nil {
		return err
	}
	if err := tasktree.CheckEnvelope(checkpoint.Envelope, checkpoint.Limits); err != nil {
		return fmt.Errorf("checkpoint envelope: %w", err)
	}
	return nil
}

func ReadCheckpoint(reader io.Reader) (Checkpoint, error) {
	var checkpoint Checkpoint
	if err := decodeObject(reader, &checkpoint); err != nil {
		return Checkpoint{}, err
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

func WriteCheckpoint(writer io.Writer, checkpoint Checkpoint) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if err := json.NewEncoder(&boundedCheckpointWriter{writer: writer, remaining: MaxInputBytes}).Encode(checkpoint); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	return nil
}

func ReadScenario(reader io.Reader) (Scenario, error) {
	var scenario Scenario
	if err := decodeObject(reader, &scenario); err != nil {
		return Scenario{}, err
	}
	if err := validateScenario(scenario, nil, false); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}
