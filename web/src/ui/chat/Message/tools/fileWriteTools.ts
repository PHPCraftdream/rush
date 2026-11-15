// Tool names whose input.content is a full file body and whose result
// metadata carries a unified diff.
// Pure code move from the former components/Message.tsx.

// ── Tool blocks ───────────────────────────────────────────────────────────────

// FileWriteTools — tool names whose input.content is the full file body and
// whose result metadata carries a unified diff. The UI hides the bulk content,
// shows just the file path on the call, and renders a coloured diff on the
// result instead of the noisy "<result>\nFile successfully written: …\n</result>"
// blob the model sees.
export const FileWriteTools = new Set(["write", "edit", "multiedit"]);
