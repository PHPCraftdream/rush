// Shared ReactMarkdown rehype plugin array — module-level so it is stable across renders.

import rehypeHighlight from "rehype-highlight";

// Stable array — never recreated
export const MD_REHYPE = [rehypeHighlight];
