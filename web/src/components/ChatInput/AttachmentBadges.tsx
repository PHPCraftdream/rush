import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Paperclip, X } from "lucide-react";
import { formatBytes } from "./formatBytes";
import type { PendingAttachment } from "./PendingAttachment";

/** Props of one attachment badge. */
interface IAttachmentBadgeProps {
  attachment: PendingAttachment;
  index: number;
  onRemove: (index: number) => void;
}

/** One pending file with its remove button. */
class AttachmentBadge extends AntiHookComponent<IAttachmentBadgeProps> {
  /** Asks the composer to drop this attachment. */
  @bind
  protected remove(): void {
    this.props.onRemove(this.props.index);
  }

  /** Renders the badge. */
  public render() {
    const { attachment } = this.props;
    return (
      <div className="attachment-badge">
        <Paperclip size={11} className="text-text-subtle shrink-0" />
        <span className="truncate max-w-[140px]">{attachment.fileName}</span>
        <span className="text-text-muted shrink-0">({formatBytes(attachment.size)})</span>
        <button
          onClick={this.remove}
          className="text-text-subtle hover:text-text transition-colors ml-0.5"
          title="Remove attachment"
        >
          <X size={11} />
        </button>
      </div>
    );
  }
}

/** Props of the badge row. */
interface IAttachmentBadgesProps {
  attachments: readonly PendingAttachment[];
  onRemove: (index: number) => void;
}

/** Badges of the files waiting to go out with the next message. */
export class AttachmentBadges extends AntiHookComponent<IAttachmentBadgesProps> {
  /**
   * Renders one badge.
   *
   * @param attachment - the pending file
   * @param idx - its position
   */
  @bind
  protected renderBadge(attachment: PendingAttachment, idx: number) {
    return <AttachmentBadge key={idx} attachment={attachment} index={idx} onRemove={this.props.onRemove} />;
  }

  /** Renders the row of badges. */
  public render() {
    return <div className="flex flex-wrap gap-2 mb-2">{this.props.attachments.map(this.renderBadge)}</div>;
  }
}
