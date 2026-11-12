import type { ChangeEvent } from "react";
import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { BatchTreeNode, BatchTreeRoot } from "../batchWork";
import { buildBatchTree } from "../batchWork";
import { batchWorkCarburetor } from "../state/batchWork";
import {
  batchNodeNotice,
  batchNodeStatus,
  batchResultMessage,
  isStopConfirmed,
  requestBatchControl,
  type BatchControlResult,
  type BatchControlTarget,
} from "./batchControls";
import type { BatchWorkRoot, BatchWorkSnapshot } from "../types";

/** Items textarea seed of a fresh node. */
const DEFAULT_ITEMS =
  '[{"task":{"kind":"generic","payload":{"tool":"run_command","input":{"program":"echo","args":["hello"]}}}}]';

/** Shared empty roots list for a session without a snapshot. */
const EMPTY_ROOTS: TReadonly<BatchWorkRoot[]> = [];

/** Refreshes the session's batch snapshot; resolves with it, or null on failure. */
type RefreshFn = () => Promise<BatchWorkSnapshot | null>;

/**
 * The operator-facing text of a thrown value.
 *
 * @param error - the thrown value
 */
const errorMessage = (error: unknown): string => (error instanceof Error ? error.message : String(error));

/**
 * Types a control reply payload as a control result.
 *
 * @param payload - the reply payload
 */
const asControlResult = (payload: unknown): BatchControlResult => payload as BatchControlResult;

/** Props of one batch node. */
interface IBatchNodeProps {
  item: BatchTreeNode;
  target: BatchControlTarget;
  refresh: RefreshFn;
}

/** Local state of one batch node. */
interface IBatchNodeState {
  open: boolean;
  busy: string;
  message: string;
  output: string;
  outputRef: string;
  items: string;
}

/** Props of a batch root section. */
interface IBatchRootSectionProps {
  batch: BatchTreeRoot;
  refresh: RefreshFn;
  className?: string;
}

/** One batch node with its Stop/Output/Add controls and its nested nodes. */
class BatchNode extends AntiHookComponent<IBatchNodeProps, IBatchNodeState> {
  public state: IBatchNodeState = {
    open: true,
    busy: "",
    message: "",
    output: "",
    outputRef: "",
    items: DEFAULT_ITEMS,
  };

  /** Collapses or expands the node's children. */
  @bind
  protected toggleOpen(): void {
    this.setState({ open: !this.state.open });
  }

  /** Requests a stop of this node. */
  @bind
  protected onStop(): void {
    void this.control("stop");
  }

  /** Requests the output of this node. */
  @bind
  protected onOutput(): void {
    void this.control("output");
  }

  /** Requests adding the typed items under this node. */
  @bind
  protected onAdd(): void {
    void this.add();
  }

  /**
   * Stores the typed items JSON.
   *
   * @param event - the textarea change event
   */
  @bind
  protected onItemsChange(event: ChangeEvent<HTMLTextAreaElement>): void {
    this.setState({ items: event.target.value });
  }

  /**
   * Runs a stop or output request and reflects the verdict.
   *
   * @param action - the control verb
   */
  protected async control(action: "stop" | "output"): Promise<void> {
    const { item, target, refresh } = this.props;
    const node = item.node;
    const nodeTarget = { ...target, nodeID: node.nodeID };
    this.setState({ busy: action, message: "" });
    if (action === "output") this.setState({ output: "", outputRef: "" });
    try {
      const reply =
        action === "stop"
          ? await requestBatchControl("stop", { ...nodeTarget, waitMS: 5000 })
          : await requestBatchControl("output", nodeTarget);
      if (reply.type !== "batch_control_result") {
        this.setState({ message: "Unexpected control response." });
        return;
      }
      const result = asControlResult(reply.payload);
      this.setState({ message: batchResultMessage(result) });
      if (action === "output" && result.ok) {
        this.setState({
          output: result.responseEnvelope || result.outcomeSummary || "",
          outputRef: result.outcomeRef || "",
        });
      }
      if (action === "stop" && result.ok) {
        const stopped = await refresh();
        const confirmed = stopped?.roots.some(
          (root) =>
            root.rootID === target.rootID &&
            root.nodes.some((candidate) => candidate.nodeID === node.nodeID && isStopConfirmed(candidate)),
        );
        if (stopped && !confirmed) {
          this.setState({
            message:
              result.result === "applied"
                ? "Stop accepted; awaiting terminal node state."
                : "Stop pending owner acknowledgement; awaiting terminal node state.",
          });
        }
      }
    } catch (error) {
      this.setState({ message: errorMessage(error) });
    } finally {
      this.setState({ busy: "" });
    }
  }

  /** Validates the typed items JSON and sends the add request. */
  protected async add(): Promise<void> {
    const { item, target, refresh } = this.props;
    const node = item.node;
    const addAllowed = node.kind === "batch" && !isStopConfirmed(node);
    if (!addAllowed) {
      this.setState({ message: "Add is available only on a live batch node." });
      return;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(this.state.items);
      if (!Array.isArray(parsed) || parsed.length === 0) throw new Error("Items must be a nonempty JSON array.");
    } catch (error) {
      this.setState({ message: error instanceof Error ? error.message : "Invalid JSON." });
      return;
    }
    this.setState({ busy: "add", message: "" });
    try {
      const reply = await requestBatchControl("add", { ...target, nodeID: node.nodeID }, parsed);
      const result = asControlResult(reply.payload);
      const accepted = reply.type === "batch_control_result";
      this.setState({ message: accepted ? batchResultMessage(result) : "Unexpected control response." });
      if (accepted && result.ok) await refresh();
    } catch (error) {
      this.setState({ message: errorMessage(error) });
    } finally {
      this.setState({ busy: "" });
    }
  }

  /**
   * Renders one in-root child node.
   *
   * @param child - the child node
   */
  @bind
  protected renderChild(child: BatchTreeNode) {
    return <BatchNode key={child.node.nodeID} item={child} target={this.props.target} refresh={this.props.refresh} />;
  }

  /**
   * Renders one nested batch owned by this node's child session.
   *
   * @param batch - the nested batch root
   */
  @bind
  protected renderChildBatch(batch: BatchTreeRoot) {
    return <BatchRootSection key={batch.root.rootID} batch={batch} refresh={this.props.refresh} />;
  }

  /** Renders the node row, its controls, output and children. */
  public render() {
    const { item } = this.props;
    const { open, busy, message, output, outputRef, items } = this.state;
    const { node, children, childBatches } = item;
    const expandable = children.length > 0 || childBatches.length > 0;
    const notice = batchNodeNotice(node);
    const terminal = isStopConfirmed(node);
    const addAllowed = node.kind === "batch" && !terminal;
    const stopPending = node.state === "cancelling";
    const label = node.taskKind || node.kind;
    return (
      <div data-batch-node={node.nodeID} className="ml-4 border-l border-surface pl-2">
        <div className="flex flex-wrap items-center gap-2 py-1">
          {expandable && (
            <button
              type="button"
              aria-expanded={open}
              aria-label={`${open ? "Collapse" : "Expand"} ${label}`}
              onClick={this.toggleOpen}
            >
              {open ? "▾" : "▸"}
            </button>
          )}
          <span>{label}</span>
          <span data-batch-state={node.state} data-test-id="batch-state" className="text-text-subtle">
            {batchNodeStatus(node)}
          </span>
          {notice && <span>{notice}</span>}
          <button
            type="button"
            aria-label={`Stop ${node.nodeID}`}
            disabled={!!busy || stopPending || terminal}
            onClick={this.onStop}
          >
            {stopPending ? "Stop pending" : "Stop"}
          </button>
          <button type="button" disabled={!!busy} onClick={this.onOutput}>Output</button>
        </div>
        {node.outcomeSummary && <div className="ml-4 text-text-subtle">{node.outcomeSummary}</div>}
        {node.heldQuestion && (
          <div
            data-test-id="held-question"
            data-held-question={node.nodeID}
            className="ml-4 border-l border-accent px-2"
          >
            {node.heldQuestion}
          </div>
        )}
        <textarea
          aria-label={`Items to add under ${node.nodeID}`}
          data-batch-add={node.nodeID}
          value={items}
          onChange={this.onItemsChange}
          rows={2}
          disabled={!addAllowed}
        />
        <button
          type="button"
          data-test-id={`batch-add-${node.nodeID}`}
          disabled={!!busy || !addAllowed}
          onClick={this.onAdd}
        >
          {busy === "add" ? "Adding…" : "Add"}
        </button>
        {message && <output data-batch-node-message={node.nodeID}>{message}</output>}
        {output && (
          <div data-test-id="batch-output">
            <pre>{output}</pre>
            {outputRef && <code>Reference: {outputRef}</code>}
          </div>
        )}
        {open && children.map(this.renderChild)}
        {open && childBatches.map(this.renderChildBatch)}
      </div>
    );
  }
}

/** A batch root: its top-level nodes addressed through the root's owner session. */
class BatchRootSection extends AntiHookComponent<IBatchRootSectionProps> {
  /**
   * Renders one top-level node of this root.
   *
   * @param item - the node
   */
  @bind
  protected renderNode(item: BatchTreeNode) {
    const { batch, refresh } = this.props;
    const target = { sessionID: batch.root.ownerSessionID, rootID: batch.root.rootID };
    return <BatchNode key={item.node.nodeID} item={item} target={target} refresh={refresh} />;
  }

  /** Renders the root's section. */
  public render() {
    const { batch, className } = this.props;
    return (
      <section data-batch-root={batch.root.rootID} className={className}>
        {batch.nodes.map(this.renderNode)}
      </section>
    );
  }
}

/** Props of the batch tree. */
interface IBatchTreeProps {
  sessionID: string;
}

/** Local state of the batch tree. */
interface IBatchTreeState {
  message: string;
  draining: boolean;
}

/** Batches tab body: the session's batch roots with Drain and Refresh controls. */
export class BatchTree extends AntiHookComponent<IBatchTreeProps, IBatchTreeState> {
  public state: IBatchTreeState = { message: "", draining: false };

  /** Detached snapshot of the session's batch roots. */
  private readonly rootsView = this.connectSelection(
    batchWorkCarburetor,
    (data) => data.bySession[this.props.sessionID]?.lastGood?.roots ?? EMPTY_ROOTS,
  );

  /** Fetches a fresh status snapshot and applies it; resolves with it, or null on failure. */
  @bind
  protected async refresh(): Promise<BatchWorkSnapshot | null> {
    const { sessionID } = this.props;
    try {
      const reply = await requestBatchControl("status", { sessionID });
      if (reply.type !== "session_batch_work") {
        this.setState({ message: "Unexpected status response." });
        return null;
      }
      const next = reply.payload as BatchWorkSnapshot;
      if (next.sessionID !== sessionID) {
        this.setState({ message: "Status response belongs to another session." });
        return null;
      }
      batchWorkCarburetor.applySnapshot(next);
      this.setState({ message: "" });
      return next;
    } catch (error) {
      this.setState({ message: errorMessage(error) });
      return null;
    }
  }

  /** Requests draining the session's batches. */
  @bind
  protected async drain(): Promise<void> {
    this.setState({ draining: true, message: "" });
    try {
      const reply = await requestBatchControl("drain", { sessionID: this.props.sessionID });
      this.setState({
        message:
          reply.type === "batch_control_result"
            ? batchResultMessage(asControlResult(reply.payload))
            : "Unexpected drain response.",
      });
    } catch (error) {
      this.setState({ message: errorMessage(error) });
    } finally {
      this.setState({ draining: false });
    }
  }

  /** Click handler of the Drain button. */
  @bind
  protected onDrain(): void {
    void this.drain();
  }

  /** Click handler of the Refresh button. */
  @bind
  protected onRefresh(): void {
    void this.refresh();
  }

  /**
   * Renders one root section.
   *
   * @param root - the root with its node tree
   */
  @bind
  protected renderRoot(root: BatchTreeRoot) {
    return <BatchRootSection key={root.root.rootID} batch={root} refresh={this.refresh} className="py-1" />;
  }

  /** Renders the controls, the status message and the root sections. */
  public render() {
    const { message, draining } = this.state;
    const tree = buildBatchTree(this.rootsView() as BatchWorkRoot[]);
    return (
      <div data-test-id="batch-tree" className="max-h-64 overflow-y-auto px-3 pb-2">
        <div className="flex items-center gap-2">
          <button type="button" disabled={draining} onClick={this.onDrain}>
            {draining ? "Draining…" : "Drain"}
          </button>
          <button type="button" onClick={this.onRefresh}>Refresh status</button>
        </div>
        {message && <output data-test-id="batch-tree-message">{message}</output>}
        {tree.map(this.renderRoot)}
        {tree.length === 0 && <div className="text-text-subtle">No batches.</div>}
      </div>
    );
  }
}
