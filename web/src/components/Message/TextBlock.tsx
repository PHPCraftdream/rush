// Plain-text (user) or markdown-rendered (assistant) text part.
// Pure code move from the former components/Message.tsx.

import { memo } from "react";
import ReactMarkdown from "react-markdown";
import { MD_REMARK, MD_REHYPE } from "./mdPlugins";

export const TextBlock = memo(function TextBlock({ text, isUser }: { text: string; isUser: boolean }) {
  if (isUser) return <span className="whitespace-pre-wrap">{text}</span>;
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
    </div>
  );
});
