package lab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

type REPLOptions struct {
	TreeKey tasktree.TreeKey
	Actor   tasktree.Actor
	Limits  tasktree.Limits
	Loaded  *Checkpoint
}

const MaxREPLLineBytes = 1024 * 1024

func REPL(ctx context.Context, input io.Reader, output io.Writer, options REPLOptions) (Checkpoint, error) {
	var empty Checkpoint
	if err := validateBinding(SchemaVersion, options.TreeKey, options.Limits); err != nil {
		return empty, err
	}
	if (options.Actor.Kind != tasktree.ActorAgent && options.Actor.Kind != tasktree.ActorOperator) || strings.TrimSpace(options.Actor.ID) == "" {
		return empty, fmt.Errorf("actor must be agent or operator with a nonblank ID")
	}
	var seeds map[tasktree.TreeKey]tasktree.Envelope
	if options.Loaded != nil {
		if err := validateCheckpoint(*options.Loaded); err != nil {
			return empty, err
		}
		if options.Loaded.TreeKey != options.TreeKey || options.Loaded.Limits != options.Limits {
			return empty, fmt.Errorf("checkpoint tree_key and limits must match REPL binding")
		}
		seeds = map[tasktree.TreeKey]tasktree.Envelope{options.TreeKey: options.Loaded.Envelope}
	}
	store, err := memory.NewStore(options.Limits, seeds)
	if err != nil {
		return empty, err
	}
	api := protocol.New(tasktree.NewService(store, options.Limits))
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), MaxREPLLineBytes)
	encoder := json.NewEncoder(output)
	line := 0
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			break
		}
		id, payload, err := readREPLWrapper(scanner.Bytes())
		if err != nil {
			return empty, fmt.Errorf("line %d: %w", line, err)
		}
		result, err := api.Execute(ctx, protocol.Invocation{
			TreeKey: options.TreeKey, Actor: options.Actor, RequestID: id,
		}, payload)
		if err != nil {
			return empty, fmt.Errorf("line %d: execute: %w", line, err)
		}
		if err := encoder.Encode(result); err != nil {
			return empty, fmt.Errorf("line %d: write result: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return empty, fmt.Errorf("read REPL: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	envelope, err := store.Load(ctx, options.TreeKey)
	if err != nil {
		return empty, err
	}
	checkpoint := Checkpoint{SchemaVersion: SchemaVersion, TreeKey: options.TreeKey, Limits: options.Limits, Envelope: envelope}
	if err := validateCheckpoint(checkpoint); err != nil {
		return empty, err
	}
	return checkpoint, nil
}

func readREPLWrapper(line []byte) (tasktree.RequestID, json.RawMessage, error) {
	if err := scanJSON(line); err != nil {
		return "", nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", nil, fmt.Errorf("expected {request_id,payload} object")
	}
	fields := make(map[string]json.RawMessage, 2)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", nil, fmt.Errorf("read wrapper key: %w", err)
		}
		key, ok := token.(string)
		if !ok || (key != "request_id" && key != "payload") {
			return "", nil, fmt.Errorf("unknown wrapper field %q", token)
		}
		if _, exists := fields[key]; exists {
			return "", nil, fmt.Errorf("duplicate wrapper field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", nil, fmt.Errorf("read wrapper value: %w", err)
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return "", nil, fmt.Errorf("close wrapper: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", nil, fmt.Errorf("unexpected trailing wrapper data")
	}
	var id tasktree.RequestID
	if err := json.Unmarshal(fields["request_id"], &id); err != nil || strings.TrimSpace(string(id)) == "" {
		return "", nil, fmt.Errorf("request_id must be a nonblank string")
	}
	payload := bytes.TrimSpace(fields["payload"])
	if len(payload) == 0 || payload[0] != '{' {
		return "", nil, fmt.Errorf("payload must be a JSON object")
	}
	return id, payload, nil
}
