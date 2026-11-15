import { AntiHookComponent } from "@react-carburetor/core";

/** Props of the busy dots. */
interface IBusyDotsProps {
  /** Name of the running smart model, when known. */
  modelName: string | null;
}

/** Pulsing dots (with the model name) shown while the active session's agent works. */
export class BusyDots extends AntiHookComponent<IBusyDotsProps> {
  /** Renders the dots. */
  public render() {
    const { modelName } = this.props;
    return (
      <div
        className="flex items-center gap-2 animate-pulse-dots px-2"
        title={modelName ? `Running ${modelName}…` : "Agent is working…"}
      >
        {modelName && <span className="text-xs text-text-subtle font-medium">{modelName}</span>}
        <span className="w-2 h-2 rounded-full bg-accent inline-block" />
        <span className="w-2 h-2 rounded-full bg-accent inline-block" />
        <span className="w-2 h-2 rounded-full bg-accent inline-block" />
      </div>
    );
  }
}
