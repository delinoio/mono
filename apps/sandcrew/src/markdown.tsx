// SPDX-License-Identifier: Apache-2.0
import type { ReactNode } from "react";

/**
 * A small Markdown renderer for agent and user messages. Agent output is
 * untrusted content, so the renderer builds React elements only: it never
 * renders raw HTML, it allows only http, https and mailto links, and links
 * open in a new tab with `rel="noopener noreferrer"`.
 *
 * It supports the subset that coding agents use most: paragraphs, headings,
 * fenced code blocks, bullet and numbered lists, block quotes, inline code,
 * bold, italic and links. Other syntax shows as plain text. A full
 * CommonMark renderer is a new dependency that needs owner approval and a
 * dependency security review (docs/apps-sandcrew-console-foundation.md).
 */
export function Markdown({ text }: { text: string }) {
  return <div className="markdown">{renderBlocks(text)}</div>;
}

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

type Block =
  | { kind: "code"; lang: string; text: string }
  | { kind: "heading"; level: number; text: string }
  | { kind: "list"; ordered: boolean; items: string[] }
  | { kind: "quote"; text: string }
  | { kind: "paragraph"; text: string };

/** Splits Markdown text into blocks. */
export function parseBlocks(text: string): Block[] {
  const lines = text.replaceAll("\r\n", "\n").split("\n");
  const blocks: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i]!;
    const fence = /^\s*(```|~~~)\s*([\w+-]*)\s*$/.exec(line);
    if (fence) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i]!.trim().startsWith(fence[1]!)) {
        body.push(lines[i]!);
        i++;
      }
      i++; // closing fence, or the end of an unclosed block
      blocks.push({
        kind: "code",
        lang: fence[2] ?? "",
        text: body.join("\n"),
      });
      continue;
    }
    if (line.trim() === "") {
      i++;
      continue;
    }
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      blocks.push({
        kind: "heading",
        level: heading[1]!.length,
        text: heading[2]!,
      });
      i++;
      continue;
    }
    const bullet = /^\s*[-*+]\s+/;
    const numbered = /^\s*\d+[.)]\s+/;
    if (bullet.test(line) || numbered.test(line)) {
      const ordered = numbered.test(line);
      const marker = ordered ? numbered : bullet;
      const items: string[] = [];
      while (i < lines.length && marker.test(lines[i]!)) {
        items.push(lines[i]!.replace(marker, ""));
        i++;
      }
      blocks.push({ kind: "list", ordered, items });
      continue;
    }
    if (/^\s*>/.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && /^\s*>/.test(lines[i]!)) {
        quoted.push(lines[i]!.replace(/^\s*>\s?/, ""));
        i++;
      }
      blocks.push({ kind: "quote", text: quoted.join("\n") });
      continue;
    }
    const paragraph: string[] = [];
    while (
      i < lines.length &&
      lines[i]!.trim() !== "" &&
      !/^\s*(```|~~~)/.test(lines[i]!) &&
      !/^#{1,6}\s/.test(lines[i]!) &&
      !bullet.test(lines[i]!) &&
      !numbered.test(lines[i]!) &&
      !/^\s*>/.test(lines[i]!)
    ) {
      paragraph.push(lines[i]!);
      i++;
    }
    blocks.push({ kind: "paragraph", text: paragraph.join("\n") });
  }
  return blocks;
}

function renderBlocks(text: string): ReactNode[] {
  return parseBlocks(text).map((block, index) => {
    switch (block.kind) {
      case "code":
        return (
          <pre key={index} className="code-block">
            <code data-lang={block.lang || undefined}>{block.text}</code>
          </pre>
        );
      case "heading": {
        // Message headings stay below the page headings.
        const Tag = block.level <= 2 ? "h3" : "h4";
        return <Tag key={index}>{renderInline(block.text)}</Tag>;
      }
      case "list": {
        const Tag = block.ordered ? "ol" : "ul";
        return (
          <Tag key={index}>
            {block.items.map((item, itemIndex) => (
              <li key={itemIndex}>{renderInline(item)}</li>
            ))}
          </Tag>
        );
      }
      case "quote":
        return <blockquote key={index}>{renderBlocks(block.text)}</blockquote>;
      case "paragraph":
        return <p key={index}>{renderInline(block.text)}</p>;
    }
  });
}

// Inline code, links, bold and italic, in order of precedence.
const INLINE =
  /(`+)([\s\S]*?[^`])\1(?!`)|\[([^\]\n]+)\]\(([^)\s]+)\)|\*\*([^*\n]+)\*\*|__([^_\n]+)__|\*([^*\n]+)\*|\b_([^_\n]+)_\b/g;

/** Renders inline Markdown as React nodes. Line breaks stay line breaks. */
export function renderInline(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let key = 0;
  const pushText = (value: string) => {
    const parts = value.split("\n");
    parts.forEach((part, index) => {
      if (index > 0) out.push(<br key={`br${key++}`} />);
      if (part !== "") out.push(part);
    });
  };
  for (const match of text.matchAll(INLINE)) {
    pushText(text.slice(last, match.index));
    last = match.index + match[0].length;
    const [, , code, label, href, bold1, bold2, italic1, italic2] = match;
    if (code !== undefined) {
      out.push(<code key={key++}>{code.trim()}</code>);
    } else if (label !== undefined && href !== undefined) {
      const safe = safeHref(href);
      out.push(
        safe === undefined ? (
          match[0]
        ) : (
          <a key={key++} href={safe} target="_blank" rel="noopener noreferrer">
            {label}
          </a>
        ),
      );
    } else if ((bold1 ?? bold2) !== undefined) {
      out.push(<strong key={key++}>{bold1 ?? bold2}</strong>);
    } else {
      out.push(<em key={key++}>{italic1 ?? italic2}</em>);
    }
  }
  pushText(text.slice(last));
  return out;
}
