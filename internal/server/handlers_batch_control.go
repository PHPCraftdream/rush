package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

// BatchControlResult is the common response envelope for WebSocket controls.
type BatchControlResult struct {
	SessionID string `json:"sessionID"`
	RootID    string `json:"rootID,omitempty"`
	NodeID    string `json:"nodeID,omitempty"`
	Action    string `json:"action"`
	Result    string `json:"result"`
	OK        bool   `json:"ok"`
}

type batchControlPayload struct {
	SessionID string          `json:"sessionID"`
	RootID    string          `json:"rootID"`
	NodeID    string          `json:"nodeID"`
	WaitMS    int             `json:"waitMS,omitempty"`
	Items     []jobbatch.Spec `json:"items,omitempty"`
}

// batchAddForWeb preserves the public App boundary while allowing focused
// validation/argument tests; production always delegates to App.BatchAdd.
var batchAddForWeb = func(ctx context.Context, a *appPkg.App, root, node string, items []jobbatch.Spec) error {
	return a.BatchAdd(ctx, root, node, items)
}

func handleBatchControl(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p batchControlPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, "session not found")
		return
	}
	switch msg.Type {
	case CmdBatchStatus:
		c.reply(msg.ID, EventSessionBatchWork, buildBatchWorkSnapshot(ctx, a, p.SessionID), "")
	case CmdBatchOutput:
		batchControlOutput(ctx, a, c, msg, p)
	case CmdBatchStop:
		batchControlStop(ctx, a, c, msg, p)
	case CmdBatchAdd:
		batchControlAdd(ctx, a, c, msg, p)
	case CmdBatchDrain:
		batchControlReply(c, msg, p, "batch_drain has no public session-scoped automatic reaction drain API; no shutdown drain was invoked", false)
	}
}

func batchControlReply(c *Client, msg WSMessage, p batchControlPayload, result string, ok bool) {
	c.reply(msg.ID, EventBatchControlResult, BatchControlResult{SessionID: p.SessionID, RootID: p.RootID, NodeID: p.NodeID, Action: msg.Type, Result: result, OK: ok}, "")
}

func batchControlOutput(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage, p batchControlPayload) {
	if p.NodeID == "" {
		batchControlReply(c, msg, p, "nodeID required", false)
		return
	}
	fac, err := a.BatchCLIStore()
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	identity, nodeID, err := fac.Store.BatchCLIFind(ctx, p.NodeID)
	if err != nil || identity.Owner != p.SessionID || (p.RootID != "" && identity.Root != p.RootID) {
		batchControlReply(c, msg, p, "batch node not found in requested session/root", false)
		return
	}
	node, err := fac.Store.BatchCLIGetNode(ctx, identity.Owner, identity.Root, nodeID)
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	if node.OutcomeSummary == "" && (!node.ResponseEnvelope.Valid || node.ResponseEnvelope.String == "") {
		batchControlReply(c, msg, p, "no retained outcome is available", false)
		return
	}
	c.reply(msg.ID, EventBatchControlResult, map[string]any{"sessionID": p.SessionID, "rootID": identity.Root, "nodeID": nodeID, "action": msg.Type, "result": "retained outcome (not a live stream)", "ok": true, "outcomeSummary": node.OutcomeSummary, "outcomeRef": node.ResponseEnvelopeRef.String, "responseEnvelope": node.ResponseEnvelope.String}, "")
}

func batchControlStop(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage, p batchControlPayload) {
	if p.NodeID == "" {
		batchControlReply(c, msg, p, "nodeID required", false)
		return
	}
	if p.WaitMS < 0 || p.WaitMS > 30000 {
		batchControlReply(c, msg, p, "waitMS must be between 0 and 30000", false)
		return
	}
	fac, err := a.BatchCLIStore()
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	identity, nodeID, err := fac.Store.BatchCLIFind(ctx, p.NodeID)
	if err != nil || identity.Owner != p.SessionID || (p.RootID != "" && identity.Root != p.RootID) {
		batchControlReply(c, msg, p, "batch node not found in requested session/root", false)
		return
	}
	target := session.BatchControlTarget{Owner: identity.Owner, Host: identity.Host, Root: identity.Root, Node: nodeID}
	req, err := fac.Store.BatchRequestStop(ctx, target, session.BatchSharedHostProbe(fac.Reader.DataDir))
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	state, ackCode := "queued", ""
	if p.WaitMS > 0 {
		wait := time.Duration(p.WaitMS) * time.Millisecond
		pollCtx, cancel := context.WithTimeout(ctx, wait)
		ack, pollErr := fac.Store.BatchCLIPollControl(pollCtx, target, req.RequestID, wait)
		cancel()
		if pollErr == nil && ack.AnswerCode != "" {
			ackCode = ack.AnswerCode
			if ackCode != "accepted" {
				batchControlReply(c, msg, p, "stop request was not accepted: "+ackCode, false)
				return
			}
			state = "applied"
		} else if pollErr != nil && !errors.Is(pollErr, context.DeadlineExceeded) {
			batchControlReply(c, msg, p, pollErr.Error(), false)
			return
		}
	}
	c.reply(msg.ID, EventBatchControlResult, map[string]any{"sessionID": p.SessionID, "rootID": identity.Root, "nodeID": nodeID, "action": msg.Type, "result": state, "ok": true, "requestID": req.RequestID, "answerCode": ackCode}, "")
}

func batchControlAdd(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage, p batchControlPayload) {
	if p.RootID == "" || p.NodeID == "" || len(p.Items) == 0 {
		batchControlReply(c, msg, p, "rootID, nodeID and nonempty items required", false)
		return
	}
	fac, err := a.BatchCLIStore()
	if err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	identity, rootNode, err := fac.Store.BatchCLIFind(ctx, p.RootID)
	if err != nil || identity.Owner != p.SessionID || rootNode != identity.Root {
		batchControlReply(c, msg, p, "root not found in requested session", false)
		return
	}
	nodeIdentity, nodeID, err := fac.Store.BatchCLIFind(ctx, p.NodeID)
	if err != nil || nodeIdentity.Owner != identity.Owner || nodeIdentity.Root != identity.Root {
		batchControlReply(c, msg, p, "node not found in requested root", false)
		return
	}
	if err = batchAddForWeb(ctx, a, identity.Root, nodeID, p.Items); err != nil {
		batchControlReply(c, msg, p, err.Error(), false)
		return
	}
	c.reply(msg.ID, EventBatchControlResult, BatchControlResult{SessionID: p.SessionID, RootID: identity.Root, NodeID: nodeID, Action: msg.Type, Result: "added", OK: true}, "")
}
