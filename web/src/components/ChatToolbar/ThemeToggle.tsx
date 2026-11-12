import { AntiHookComponent, bind } from "@react-carburetor/core";
import { Moon, Sun } from "lucide-react";
import { setTheme } from "../../store";
import { configCarburetor } from "../../state/config";

/** Light/dark switch of the toolbar. */
export class ThemeToggle extends AntiHookComponent {
  /** Flips the theme. */
  @bind
  protected toggle(): void {
    setTheme(configCarburetor.getData().config?.theme === "dark" ? "light" : "dark");
  }

  /** Renders the button. */
  public render() {
    const isDark = this.useCarburetor(configCarburetor).config?.theme === "dark";
    return (
      <button
        data-test-id="header-theme-toggle"
        onClick={this.toggle}
        title={isDark ? "Switch to light theme" : "Switch to dark theme"}
        className={
          "flex items-center justify-center w-8 h-8 rounded-lg border transition-colors " +
          "bg-base-overlay border-surface text-text-subtle hover:border-accent/50 hover:text-text"
        }
      >
        {isDark ? <Sun size={14} /> : <Moon size={14} />}
      </button>
    );
  }
}
