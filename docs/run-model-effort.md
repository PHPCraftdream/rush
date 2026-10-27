# Non-interactive model/effort isolation

`rush run --model`, `--fast-model`, and `--effort` select values for that invocation only. They do not rewrite a session's saved model or reasoning-effort preferences. A later run attached to the same session does not inherit those command-line overrides unless they are supplied again. This keeps automation runs from changing choices later used by interactive sessions.

This is specifically about model and effort overrides; it does not imply that all `rush run` options are transient. System-prompt/session behavior is governed separately.

The web session model selectors remain session preferences: web-selected models and effort levels continue to be saved and applied to subsequent turns.
