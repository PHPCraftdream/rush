package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

var errBatchCallCredentialsRequired = errors.New("batch call credentials marker requires launch credentials")

func stampBatchCallCredentials(ctx context.Context, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if spec.Task != nil {
		if spec.Task.Kind != batchKindAgent {
			return spec, nil
		}
		payload := append(json.RawMessage(nil), spec.Task.Payload...)
		var p batchLeafPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return jobbatch.Spec{}, err
		}
		if p.Tool == "agent" || p.Tool == "agentic_fetch" {
			if p.CallCredentials && callCredentialsFrom(ctx) == nil {
				return jobbatch.Spec{}, errBatchCallCredentialsRequired
			}
			if callCredentialsFrom(ctx) != nil {
				p.CallCredentials = true
				raw, err := json.Marshal(p)
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
		stamped, err := stampBatchCallCredentials(ctx, batch.Items[i])
		if err != nil {
			return jobbatch.Spec{}, err
		}
		batch.Items[i] = stamped
	}
	spec.Batch = &batch
	return spec, nil
}
