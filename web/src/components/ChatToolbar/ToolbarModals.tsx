import { AntiHookComponent } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import type { Session } from "../../types";
import { LogsModal } from "../LogsModal";
import { MCPSettings } from "../MCPSettings";
import { ProvidersModal } from "../ProvidersModal";
import { ScopedModelsModal } from "../ScopedModelsModal";
import { SettingsModal } from "../SettingsModal";
import { ShutdownConfirm } from "./ShutdownConfirm";
import { SystemPromptModal } from "./SystemPromptModal";
import type { ToolbarModal } from "./ToolbarModal";

/** Props of the modal host. */
interface IToolbarModalsProps {
  modal: ToolbarModal | null;
  activeSessionID: string | null;
  activeSession: TReadonly<Session> | null;
  onClose: () => void;
}

/** Hosts whichever toolbar modal is open. */
export class ToolbarModals extends AntiHookComponent<IToolbarModalsProps> {
  /** Renders the open modal, if any. */
  public render() {
    const { modal, activeSessionID, onClose } = this.props;
    switch (modal) {
      case "prompt":
        return activeSessionID ? <SystemPromptModal sessionID={activeSessionID} onClose={onClose} /> : null;
      case "mcp":
        return <MCPSettings onClose={onClose} />;
      case "settings":
        return <SettingsModal onClose={onClose} />;
      case "providers":
        return <ProvidersModal onClose={onClose} />;
      case "scoped":
        // The cast hands the detached read-only session to a modal that only reads it.
        return <ScopedModelsModal onClose={onClose} activeSession={this.props.activeSession as Session | null} />;
      case "logs":
        return <LogsModal onClose={onClose} />;
      case "shutdown":
        return <ShutdownConfirm onClose={onClose} />;
      default:
        return null;
    }
  }
}
