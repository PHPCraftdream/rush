// old_string -> new_string red/green preview for edit/multiedit calls.
// Pure code move from the former components/Message.tsx.

import { memo } from "react";

// EditPreview — renders old_string→new_string as red/green lines so the
// operator sees the intent of an edit at a glance without expanding raw JSON.
export const EditPreview = memo(function EditPreview({ old, new_ }: { old: string; new_: string }) {
  const oldLines = old.split("\n");
  const newLines = new_.split("\n");
  return (
    <pre className="diff-body text-xs mt-1">
      {oldLines.map((l, i) => (
        <div key={`d${i}`} className="diff-line diff-del">-{l}</div>
      ))}
      {newLines.map((l, i) => (
        <div key={`a${i}`} className="diff-line diff-add">+{l}</div>
      ))}
    </pre>
  );
});
