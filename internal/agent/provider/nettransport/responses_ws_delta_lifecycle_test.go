package nettransport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesWSDeltaOlderCompletionContradictionKeepsConnection(t *testing.T) {
	wire := &responsesWSTestWire{read: [][]byte{
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[]}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`),
	}}
	conn := newResponsesWSTestConn(wire, 2048, 4096)
	conn.protocol = "public-openai-responses"
	conn.deltaEnabled = true
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(context.Background(), []byte(`{"type":"response.create","stream":true,"store":false,"input":[{"role":"user"}]}`)))
	_, err := conn.ReceiveRaw(context.Background())
	require.NoError(t, err)
	_, err = conn.ReceiveRaw(context.Background())
	require.NoError(t, err)
	require.NoError(t, conn.MarkCompleted())
	require.True(t, conn.Reusable())
	require.Nil(t, conn.delta.seed)
	require.False(t, wire.closed)
}
