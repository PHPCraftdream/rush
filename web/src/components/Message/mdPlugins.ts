// Shared ReactMarkdown plugin arrays — module-level so the arrays are stable
// across renders. Pure code move from the former components/Message.tsx.

import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";
import rehypeHighlight from "rehype-highlight";

// Stable arrays — never recreated
export const MD_REMARK = [remarkGfm, remarkBreaks];
export const MD_REHYPE = [rehypeHighlight];
