// old_string -> new_string red/green preview for edit/multiedit calls.

import { memo } from "react";

/** Props of the edit preview. */
interface IEditPreviewProps {
  old: string;
  new_: string;
}

/**
 * Renders one removed line.
 *
 * @param line - the old line text
 * @param i - the line index
 */
const renderOldLine = (line: string, i: number) => (
  <div key={`d${i}`} className="diff-line diff-del">-{line}</div>
);

/**
 * Renders one added line.
 *
 * @param line - the new line text
 * @param i - the line index
 */
const renderNewLine = (line: string, i: number) => (
  <div key={`a${i}`} className="diff-line diff-add">+{line}</div>
);

/**
 * Renders old_string→new_string as red/green lines so the operator sees the
 * intent of an edit at a glance without expanding raw JSON.
 */
export const EditPreview = memo(function EditPreview({ old, new_ }: IEditPreviewProps) {
  return (
    <pre className="diff-body text-xs mt-1">
      {old.split("\n").map(renderOldLine)}
      {new_.split("\n").map(renderNewLine)}
    </pre>
  );
});
