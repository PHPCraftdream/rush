package batchadapter

import (
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// DecodeCredentialMarker describes a leaf and returns its typed re-encoder.
type DecodeCredentialMarker func(json.RawMessage) (tool string, marked bool, mark func() (json.RawMessage, error), err error)

var ErrCallCredentialsRequired = errors.New("batch call credentials marker requires launch credentials")

func StampCallCredentials(spec jobbatch.Spec, agentKind string, hasCredentials bool, decode DecodeCredentialMarker) (jobbatch.Spec, error) {
	if spec.Task != nil {
		if spec.Task.Kind != agentKind {
			return spec, nil
		}
		tool, marked, mark, err := decode(append(json.RawMessage(nil), spec.Task.Payload...))
		if err != nil {
			return jobbatch.Spec{}, err
		}
		if tool == "agent" || tool == "agentic_fetch" {
			if marked && !hasCredentials {
				return jobbatch.Spec{}, ErrCallCredentialsRequired
			}
			if hasCredentials {
				raw, err := mark()
				if err != nil {
					return jobbatch.Spec{}, err
				}
				// Copy the whole TaskSpec and replace only the payload: Timeout
				// (and any future field) must survive the stamping.
				task := *spec.Task
				task.Payload = raw
				spec.Task = &task
			}
		}
		return spec, nil
	}
	if spec.Batch == nil {
		return spec, nil
	}
	batch := *spec.Batch
	batch.Items = append([]jobbatch.Spec(nil), spec.Batch.Items...)
	for i := range batch.Items {
		stamped, err := StampCallCredentials(batch.Items[i], agentKind, hasCredentials, decode)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		batch.Items[i] = stamped
	}
	spec.Batch = &batch
	return spec, nil
}

// DecodeCredentialRegistration defers key construction until a marked launch
// has credentials to escrow.
type DecodeCredentialRegistration func(json.RawMessage) (tool string, marked bool, key func() CredentialKey, err error)

func CollectCredentialRegistrations(root string, spec jobbatch.Spec, agentKind string, creds *agentcall.CredentialSet, decode DecodeCredentialRegistration) ([]CredentialRegistration, error) {
	if spec.Task != nil {
		if spec.Task.Kind != agentKind {
			return nil, nil
		}
		tool, marked, key, err := decode(spec.Task.Payload)
		if err != nil {
			return nil, err
		}
		if (tool != "agent" && tool != "agentic_fetch") || !marked {
			return nil, nil
		}
		if creds == nil {
			return nil, ErrCredentialEscrowMissing
		}
		return []CredentialRegistration{{Key: key(), Root: root, Credentials: creds}}, nil
	}
	if spec.Batch == nil {
		return nil, jobbatch.ErrInvalidSpec
	}
	var all []CredentialRegistration
	for _, item := range spec.Batch.Items {
		entries, err := CollectCredentialRegistrations(root, item, agentKind, creds, decode)
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
	}
	return all, nil
}
