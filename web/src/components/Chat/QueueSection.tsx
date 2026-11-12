import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { QueuedMessage } from "../../store";
import { QueuedMessageItem } from "./QueuedMessageItem";

/** Props of the queue section. */
interface IQueueSectionProps {
  items: TReadonly<QueuedMessage[]>;
  sessionID: string;
  sessionBusy: boolean;
}

/** The client-side message queue shown above the composer. */
export class QueueSection extends AntiHookComponent<IQueueSectionProps> {
  /**
   * Renders one queued row.
   *
   * @param item - the queued message
   * @param idx - its zero-based position
   */
  @bind
  protected renderItem(item: TReadonly<QueuedMessage>, idx: number) {
    const { items, sessionID, sessionBusy } = this.props;
    return (
      <QueuedMessageItem
        key={item.id}
        item={item}
        sessionID={sessionID}
        position={idx + 1}
        total={items.length}
        sessionBusy={sessionBusy}
      />
    );
  }

  /** Renders the header and the rows. */
  public render() {
    return (
      <div className="shrink-0 border-t border-surface bg-canvas">
        <div className="flex items-center gap-2 px-5 py-1.5">
          <div className="divider-line" />
          <span className="section-label">Queue · {this.props.items.length}</span>
          <div className="divider-line" />
        </div>
        {this.props.items.map(this.renderItem)}
      </div>
    );
  }
}
