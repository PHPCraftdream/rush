// Plain-text (user) or markdown-rendered (assistant) text part.

import { memo } from "react";
import ReactMarkdown from "react-markdown";
import { MD_REMARK } from "./mdRemarkPlugins";
import { MD_REHYPE } from "./mdRehypePlugins";

/** Props of the text block. */
interface ITextBlockProps {
  text: string;
  isUser: boolean;
}

/** User text renders verbatim; assistant text renders as markdown. */
export const TextBlock = memo(function TextBlock({ text, isUser }: ITextBlockProps) {
  if (isUser) return <span className="whitespace-pre-wrap">{text}</span>;
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={MD_REMARK} rehypePlugins={MD_REHYPE}>{text}</ReactMarkdown>
    </div>
  );
});
