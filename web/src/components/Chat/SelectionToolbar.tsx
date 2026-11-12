import { AntiHookComponent } from "@react-carburetor/core";
import { Trash2, X } from "lucide-react";
import { clearSelection } from "./clearSelection";

/** Props of the selection toolbar. */
interface ISelectionToolbarProps {
  count: number;
  onDeleteSelected: () => void;
}

/** Bar shown while messages are ticked: delete them or cancel the selection. */
export class SelectionToolbar extends AntiHookComponent<ISelectionToolbarProps> {
  /** Renders the count, the delete action and the cancel action. */
  public render() {
    return (
      <div className="selection-toolbar">
        <span className="text-sm text-text-subtle">{this.props.count} selected</span>
        <button onClick={this.props.onDeleteSelected} className="btn-delete">
          <Trash2 size={14} />
          Delete selected
        </button>
        <button
          onClick={clearSelection}
          className="ml-auto flex items-center gap-1.5 text-sm text-text-subtle hover:text-text transition-colors"
        >
          <X size={14} />
          Cancel
        </button>
      </div>
    );
  }
}
