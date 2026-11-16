// Shared ReactMarkdown remark plugin array — module-level so it is stable across renders.

import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";

// Stable array — never recreated
export const MD_REMARK = [remarkGfm, remarkBreaks];
