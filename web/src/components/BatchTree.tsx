import { useState } from "react";
import { useStore } from "@nanostores/react";
import type { BatchTreeNode } from "../batchWork";
import { $batchWorkBySession, applyBatchWorkSnapshot, buildBatchTree, pickBatchWork } from "../batchWork";
import { batchNodeNotice, batchNodeStatus, batchResultMessage, isStopConfirmed, requestBatchControl, type BatchControlTarget } from "./batchControls";
import type { BatchWorkSnapshot } from "../types";

export function BatchTree({ sessionID }: { sessionID: string }) {
  const batchMap = useStore($batchWorkBySession);
  const snapshot = pickBatchWork(batchMap, sessionID)?.lastGood;
  const tree = buildBatchTree(snapshot?.roots ?? []);
  const [message, setMessage] = useState("");
  const [draining, setDraining] = useState(false);

  async function refresh(): Promise<BatchWorkSnapshot | null> {
    try {
      const reply = await requestBatchControl("status", { sessionID });
      if (reply.type !== "session_batch_work") { setMessage("Unexpected status response."); return null; }
      const snapshot = reply.payload as BatchWorkSnapshot;
      if (snapshot.sessionID !== sessionID) { setMessage("Status response belongs to another session."); return null; }
      applyBatchWorkSnapshot(snapshot);
      setMessage("");
      return snapshot;
    } catch (error) { setMessage(error instanceof Error ? error.message : String(error)); return null; }
  }

  async function drain() {
    setDraining(true); setMessage("");
    try {
      const reply = await requestBatchControl("drain", { sessionID });
      setMessage(reply.type === "batch_control_result" ? batchResultMessage(reply.payload as Parameters<typeof batchResultMessage>[0]) : "Unexpected drain response.");
    } catch (error) { setMessage(error instanceof Error ? error.message : String(error)); }
    finally { setDraining(false); }
  }

  return <div data-test-id="batch-tree" className="max-h-64 overflow-y-auto px-3 pb-2">
    <div className="flex items-center gap-2">
      <button type="button" disabled={draining} onClick={() => void drain()}>{draining ? "Draining…" : "Drain"}</button>
      <button type="button" onClick={() => void refresh()}>Refresh status</button>
    </div>
    {message && <div role="status" data-test-id="batch-tree-message">{message}</div>}
    {tree.map((root) => <section key={root.root.rootID} data-batch-root={root.root.rootID} className="py-1">{root.nodes.map((item) => <BatchNode key={item.node.nodeID} item={item} target={{ sessionID: root.root.ownerSessionID, rootID: root.root.rootID }} refresh={refresh} />)}</section>)}
    {tree.length === 0 && <div className="text-text-subtle">No batches.</div>}
  </div>;
}

function BatchNode({ item, target, refresh }: { item: BatchTreeNode; target: BatchControlTarget; refresh: () => Promise<BatchWorkSnapshot | null> }) {
  const [open, setOpen] = useState(true);
  const [busy, setBusy] = useState("");
  const [message, setMessage] = useState("");
  const [output, setOutput] = useState("");
  const [outputRef, setOutputRef] = useState("");
  const [items, setItems] = useState('[{"task":{"kind":"generic","payload":{"tool":"run_command","input":{"program":"echo","args":["hello"]}}}}]');
  const { node, children, childBatches } = item;
  const nodeTarget = { ...target, nodeID: node.nodeID };
  const expandable = children.length > 0 || childBatches.length > 0;
  const notice = batchNodeNotice(node);
  const terminal = isStopConfirmed(node);
  const addAllowed = node.kind === "batch" && !terminal;

  async function control(action: "stop" | "output") {
    setBusy(action); setMessage("");
    if (action === "output") { setOutput(""); setOutputRef(""); }
    try {
      const reply = action === "stop"
        ? await requestBatchControl("stop", { ...nodeTarget, waitMS: 5000 })
        : await requestBatchControl("output", nodeTarget);
      if (reply.type !== "batch_control_result") { setMessage("Unexpected control response."); return; }
      const result = reply.payload as Parameters<typeof batchResultMessage>[0];
      setMessage(batchResultMessage(result));
      if (action === "output" && result.ok) {
        setOutput(result.responseEnvelope || result.outcomeSummary || "");
        setOutputRef(result.outcomeRef || "");
      }
      if (action === "stop" && result.ok) {
        const stopped = await refresh();
        if (stopped && !stopped.roots.some((root) => root.rootID === target.rootID && root.nodes.some((candidate) => candidate.nodeID === node.nodeID && isStopConfirmed(candidate)))) {
          setMessage(result.result === "applied" ? "Stop accepted; awaiting terminal node state." : "Stop pending owner acknowledgement; awaiting terminal node state.");
        }
      }
    } catch (error) { setMessage(error instanceof Error ? error.message : String(error)); }
    finally { setBusy(""); }
  }

  async function add() {
    if (!addAllowed) { setMessage("Add is available only on a live batch node."); return; }
    let parsed: unknown;
    try {
      parsed = JSON.parse(items);
      if (!Array.isArray(parsed) || parsed.length === 0) throw new Error("Items must be a nonempty JSON array.");
    } catch (error) { setMessage(error instanceof Error ? error.message : "Invalid JSON."); return; }
    setBusy("add"); setMessage("");
    try {
      const reply = await requestBatchControl("add", nodeTarget, parsed);
      const result = reply.payload as Parameters<typeof batchResultMessage>[0];
      setMessage(reply.type === "batch_control_result" ? batchResultMessage(result) : "Unexpected control response.");
      if (reply.type === "batch_control_result" && result.ok) await refresh();
    } catch (error) { setMessage(error instanceof Error ? error.message : String(error)); }
    finally { setBusy(""); }
  }

  const stopPending = node.state === "cancelling";
  return <div data-batch-node={node.nodeID} className="ml-4 border-l border-surface pl-2">
    <div className="flex flex-wrap items-center gap-2 py-1">
      {expandable && <button type="button" aria-expanded={open} aria-label={`${open ? "Collapse" : "Expand"} ${node.taskKind || node.kind}`} onClick={() => setOpen(!open)}>{open ? "▾" : "▸"}</button>}
      <span>{node.taskKind || node.kind}</span><span data-batch-state={node.state} data-test-id="batch-state" className="text-text-subtle">{batchNodeStatus(node)}</span>
      {notice && <span>{notice}</span>}
      <button type="button" aria-label={`Stop ${node.nodeID}`} disabled={!!busy || stopPending || terminal} onClick={() => void control("stop")}>{stopPending ? "Stop pending" : "Stop"}</button>
      <button type="button" disabled={!!busy} onClick={() => void control("output")}>Output</button>
    </div>
    {node.outcomeSummary && <div className="ml-4 text-text-subtle">{node.outcomeSummary}</div>}
    {node.heldQuestion && <div data-test-id="held-question" data-held-question={node.nodeID} className="ml-4 border-l border-accent px-2">{node.heldQuestion}</div>}
    <textarea aria-label={`Items to add under ${node.nodeID}`} data-batch-add={node.nodeID} value={items} onChange={(event) => setItems(event.target.value)} rows={2} disabled={!addAllowed} />
    <button type="button" data-test-id={`batch-add-${node.nodeID}`} disabled={!!busy || !addAllowed} onClick={() => void add()}>{busy === "add" ? "Adding…" : "Add"}</button>
    {message && <div role="status" data-batch-node-message={node.nodeID}>{message}</div>}
    {output && <div data-test-id="batch-output"><pre>{output}</pre>{outputRef && <code>Reference: {outputRef}</code>}</div>}
    {open && children.map((child) => <BatchNode key={child.node.nodeID} item={child} target={target} refresh={refresh} />)}
    {open && childBatches.map((batch) => <section key={batch.root.rootID} data-batch-root={batch.root.rootID}>{batch.nodes.map((nested) => <BatchNode key={nested.node.nodeID} item={nested} target={{ sessionID: batch.root.ownerSessionID, rootID: batch.root.rootID }} refresh={refresh} />)}</section>)}
  </div>;
}
