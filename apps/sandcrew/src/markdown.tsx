// SPDX-License-Identifier: Apache-2.0
import type { ComponentProps } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

const SAFE_PROTOCOLS: ReadonlySet<string> = new Set([
  "http:",
  "https:",
  "mailto:",
]);

/** Returns the URL when its protocol is allowed, otherwise undefined. */
export function safeHref(raw: string): string | undefined {
  try {
    const url = new URL(raw);
    return SAFE_PROTOCOLS.has(url.protocol) ? url.href : undefined;
  } catch {
    return undefined;
  }
}

/** Opens agent links in a new tab without access to the console window. */
function ExternalLink({
  node: _node,
  ...props
}: ComponentProps<"a"> & { node?: unknown }) {
  return <a {...props} target="_blank" rel="noopener noreferrer" />;
}

/** Message headings stay below the headings of the page. */
function MessageHeading({
  node: _node,
  ...props
}: ComponentProps<"h3"> & { node?: unknown }) {
  return <h3 {...props} />;
}

function MessageSubheading({
  node: _node,
  ...props
}: ComponentProps<"h4"> & { node?: unknown }) {
  return <h4 {...props} />;
}

function CodeBlock({
  node: _node,
  ...props
}: ComponentProps<"pre"> & { node?: unknown }) {
  return <pre {...props} className="code-block" />;
}

const COMPONENTS: Components = {
  a: ExternalLink,
  h1: MessageHeading,
  h2: MessageHeading,
  h3: MessageSubheading,
  h4: MessageSubheading,
  h5: MessageSubheading,
  h6: MessageSubheading,
  pre: CodeBlock,
};

/**
 * Renders a user or agent message as GitHub Flavored Markdown with
 * `react-markdown` and `remark-gfm`. Agent output is untrusted content:
 * raw HTML is dropped, links and images keep only http, https and mailto
 * URLs, and links open with `rel="noopener noreferrer"`.
 */
export function Markdown({ text }: { text: string }) {
  return (
    <div className="markdown">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        urlTransform={(url) => safeHref(url)}
        components={COMPONENTS}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
