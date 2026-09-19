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
