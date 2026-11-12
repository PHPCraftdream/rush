import { AntiHookComponent } from "@react-carburetor/core";
import type { TReadonly } from "@react-carburetor/core";
import { ChevronDown } from "lucide-react";
import type { Session } from "../../types";
import { ModelSelector } from "../ModelSelector";

/** Props of the model selector cluster. */
interface IModelClusterProps {
  activeSession: TReadonly<Session> | null;
  /**
   * Worker/reviewer selectors are collapsed by default (task #1061): most
   * sessions never set them, so always rendering both next to smart/fast
   * would overload the toolbar for the common case that has nothing to show.
   */
  showRoleModels: boolean;
  onToggleRoleModels: () => void;
}

/** The right-hand cluster: smart/fast selectors, plus worker/reviewer behind a toggle. */
export class ModelCluster extends AntiHookComponent<IModelClusterProps> {
  /** Renders the selectors and the expand toggle. */
  public render() {
    const { showRoleModels, onToggleRoleModels } = this.props;
    // The cast hands the detached read-only session to selectors that only read it.
    const session = this.props.activeSession as Session | null;
    return (
      <>
        {showRoleModels && session && (
          <>
            <ModelSelector session={session} modelType="worker" />
            <ModelSelector session={session} modelType="reviewer" />
          </>
        )}
        <ModelSelector session={session} modelType="smart" />
        <ModelSelector session={session} modelType="fast" />
        {session && (
          <button
            onClick={onToggleRoleModels}
            data-test-id="toggle-role-models"
            title={showRoleModels ? "Hide worker/reviewer model selectors" : "Show worker/reviewer model selectors"}
            className="p-1.5 rounded-lg text-text-subtle hover:text-text hover:bg-base-overlay transition-colors"
          >
            <ChevronDown size={13} className={`transition-transform ${showRoleModels ? "rotate-180" : ""}`} />
          </button>
        )}
      </>
    );
  }
}
