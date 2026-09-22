// formatActionArgs — one-line preview of a tool call's most useful argument.
// Shared by the main accordion (Message/ActionRow.tsx) and sub-agent blocks
// (components/SubAgentBlock.tsx) so both surfaces show WHICH file/command
// each tool call touched, not just its bare name.
//
// Moved out of ActionRow.tsx (was private there) when SubAgentBlock needed
// the same logic — a hand-copied second version is exactly how the two
// surfaces would have drifted.
export function formatActionArgs(name: string, input: string): string {
  if (!input) return "";
  let parsed: Record<string, unknown> = {};
  try { parsed = JSON.parse(input) as Record<string, unknown>; } catch { return ""; }
  // JSON.parse also succeeds for non-object payloads ("null", "42",
  // '"text"', "[1,2]"), and everything below assumes an object: indexing
  // parsed[k] on null throws a TypeError out here, past the catch. Treat
  // any non-object (null, scalar, array) as "no structured input" — a bare
  // JSON string previews itself, anything else shows nothing.
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return typeof parsed === "string" ? parsed : "";
  }
  const s = (k: string) => typeof parsed[k] === "string" ? (parsed[k] as string) : "";
  switch (name) {
    case "bash":      return s("command");
    case "view":      return s("file_path") || s("path") || s("filePath");
    case "write":
    case "edit":
    case "multiedit": return s("file_path");
    case "glob":      return s("pattern");
    case "grep":      return [s("pattern"), s("path")].filter(Boolean).join(" · ");
    case "ls":        return s("path");
    case "fetch":     return s("url");
    case "download":  return s("url");
    case "agent":     return s("prompt") || s("description");
    default: {
      // First string value in the object as a sensible fallback.
      for (const v of Object.values(parsed)) if (typeof v === "string" && v) return v;
      return "";
    }
  }
}
