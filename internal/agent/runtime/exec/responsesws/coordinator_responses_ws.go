package responsesws

import (
	"context"
	"errors"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/nettransport"
	"github.com/PHPCraftdream/rush/internal/config"
)

func NewCodexTransport(manager *nettransport.ResponsesWSManager, current func() (codexprovider.WebSocketPolicy, config.NetworkConfig, error)) codexprovider.WebSocketManager {
	return &codexWebSocketTransport{ResponsesWSManager: manager, current: current}
}

type codexWebSocketTransport struct {
	*nettransport.ResponsesWSManager
	current func() (codexprovider.WebSocketPolicy, config.NetworkConfig, error)
}

func (t *codexWebSocketTransport) Acquire(ctx context.Context, request codexprovider.WebSocketRequest) (codexprovider.WebSocketLease, error) {
	policy, network, err := t.current()
	if err != nil {
		return nil, err
	}
	if policy != request.Policy {
		return nil, errors.New("openai-codex connection policy changed before handshake")
	}
	lease, err := t.ResponsesWSManager.Acquire(ctx, nettransport.ResponsesWSOptions{
		Protocol: "codex-responses", Endpoint: request.Endpoint, ReadPolicy: nettransport.ResponsesWSReadRequestContext,
		AuthToken: request.AuthToken, Headers: request.Headers, Network: network, StillCurrent: request.StillCurrent,
		Owner: nettransport.ResponsesWSOwner{AccountID: request.AccountID, AuthRevision: request.AuthRevision, ConversationID: request.ConversationID},
	}, nettransport.ResponsesWSPlannedMaxAge)
	if err != nil {
		return nil, err
	}
	return codexWebSocketLease{lease}, nil
}

func (*codexWebSocketTransport) UnauthorizedHandshake(err error) bool {
	var handshake *nettransport.ResponsesWSHandshakeError
	return errors.As(err, &handshake) && handshake.Status == http.StatusUnauthorized
}

type codexWebSocketLease struct{ *nettransport.ResponsesWSLease }

func (l codexWebSocketLease) Conn() codexprovider.WebSocketConnection {
	return l.ResponsesWSLease.Conn()
}

var (
	_ codexprovider.WebSocketManager       = (*codexWebSocketTransport)(nil)
	_ codexprovider.WebSocketExchangeError = (*nettransport.ResponsesWSError)(nil)
)
