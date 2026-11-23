package agent

import (
	"context"
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

func stampBatchCallCredentials(ctx context.Context, spec jobbatch.Spec) (jobbatch.Spec, error) {
	return batchadapter.StampCallCredentials(spec, batchdispatch.KindAgent, callCredentialsFrom(ctx) != nil, decodeBatchCredentialMarker)
}

func decodeBatchCredentialMarker(raw json.RawMessage) (string, bool, func() (json.RawMessage, error), error) {
	var p batchLeafPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false, nil, err
	}
	return p.Tool, p.CallCredentials, func() (json.RawMessage, error) {
		p.CallCredentials = true
		return json.Marshal(p)
	}, nil
}

func decodeBatchCredentialRegistration(raw json.RawMessage) (string, bool, func() batchadapter.CredentialKey, error) {
	var p batchLeafPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false, nil, err
	}
	return p.Tool, p.CallCredentials, func() batchadapter.CredentialKey { return batchCredentialKeyFor(p).adapterKey() }, nil
}
