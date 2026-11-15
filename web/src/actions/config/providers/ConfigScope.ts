/**
 * Where a config write lands: "global" or "local" (workspace).
 *
 * Omitted/undefined defaults to "global" server-side, matching every
 * scope-aware CLI command.
 */
export type ConfigScope = "global" | "local";
