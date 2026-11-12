import { AntiHookComponent, bind } from "@react-carburetor/core";
import type { ConfigScope } from "../../store";

/** Props of one scope button. */
interface IScopeButtonProps {
  scope: ConfigScope;
  active: boolean;
  testId: string;
  onPick: (scope: ConfigScope) => void;
}

/** One scope choice of the toggle. */
class ScopeButton extends AntiHookComponent<IScopeButtonProps> {
  /** Reports this scope as chosen. */
  @bind
  protected pick(): void {
    this.props.onPick(this.props.scope);
  }

  /** Renders the button. */
  public render() {
    const { scope, active, testId } = this.props;
    return (
      <button
        type="button"
        onClick={this.pick}
        data-test-id={testId}
        className={`px-3 py-1 text-xs rounded-md transition-colors capitalize ${
          active ? "bg-accent-fill text-white/90" : "text-text-subtle hover:text-text"
        }`}
      >
        {scope}
      </button>
    );
  }
}

/** Props of the scope toggle. */
interface IScopeToggleProps {
  /** Test id of the container; each button adds `-<scope>`. */
  testId: string;
  scope: ConfigScope;
  onChange: (scope: ConfigScope) => void;
}

/** Global/local switch for where a write lands. */
export class ScopeToggle extends AntiHookComponent<IScopeToggleProps> {
  /** Renders the two scope buttons. */
  public render() {
    const { testId, scope, onChange } = this.props;
    return (
      <div className="flex gap-1 p-0.5 bg-canvas border border-surface rounded-lg w-fit" data-test-id={testId}>
        <ScopeButton scope="global" active={scope === "global"} testId={`${testId}-global`} onPick={onChange} />
        <ScopeButton scope="local" active={scope === "local"} testId={`${testId}-local`} onPick={onChange} />
      </div>
    );
  }
}
