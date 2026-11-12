import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Headphones } from "lucide-react";
import { setKeepAliveEnabled } from "../../store";
import { configCarburetor } from "../../state/config";

/**
 * Default ON when the server omits the field (older backend) or sends
 * explicit true; OFF only when the operator has explicitly disabled it.
 *
 * @param keepAliveEnabled - the server's preference, if it sent one
 */
const isKeepAliveOn = (keepAliveEnabled: boolean | undefined): boolean => keepAliveEnabled !== false;

/**
 * BT keep-alive: tiny inaudible WebAudio loop that prevents
 * Bluetooth headphones from suspending the audio device during
 * long agent runs (otherwise they eat the first second of any
 * real notification). Backed by Options.KeepAliveEnabled in the
 * global rush.json; default ON.
 */
export class KeepAliveToggle extends AntiHookComponent {
  /** Flips the preference. */
  @bind
  protected toggle(): void {
    setKeepAliveEnabled(!isKeepAliveOn(configCarburetor.getData().config?.keepAliveEnabled));
  }

  /** Renders the button. */
  public render() {
    const keepAliveOn = isKeepAliveOn(this.useCarburetor(configCarburetor).config?.keepAliveEnabled);
    return (
      <button
        data-test-id="header-keepalive-toggle"
        onClick={this.toggle}
        title={
          keepAliveOn
            ? "BT keep-alive ON — inaudible audio loop keeps Bluetooth headphones awake. Click to disable."
            : "BT keep-alive OFF — Bluetooth headphones may suspend during silent periods. Click to enable."
        }
        className={`flex items-center justify-center w-8 h-8 rounded-lg border transition-colors ${
          keepAliveOn
            ? "bg-yellow/10 border-yellow/40 text-yellow hover:bg-yellow/20"
            : "bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        }`}
      >
        <Headphones size={14} />
      </button>
    );
  }
}
