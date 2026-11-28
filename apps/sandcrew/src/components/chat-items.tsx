// SPDX-License-Identifier: Apache-2.0
import type {
  ContentBlock,
  Diff,
  PermissionOption,
  ToolCallContent,
} from "@delino/sandcrew-protos/v1/bridge_pb";
import {
  PermissionOptionKind,
  PermissionOutcome,
  ToolCallStatus,
} from "@delino/sandcrew-protos/v1/common_pb";
import { BadgeVariant } from "delino-ui/badge";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import {
  Collapsible,
  CollapsiblePanel,
  CollapsibleTrigger,
} from "delino-ui/collapsible";
import type { ReactNode } from "react";
import { diffLines, DiffLineKind } from "../diff.ts";
import { labels } from "../format.ts";
import { Markdown, safeHref } from "../markdown.tsx";
import {
  type ChatItem,
  ChatItemKind,
  type PermissionItem,
  type ToolCallItem,
} from "../transcript.ts";
import { StateBadge } from "./ui.tsx";

/** What a permission request can do in the current view. */
export interface PermissionActions {
  /** Answers a pending request; unset when the user cannot answer. */
  answer?: (permissionRequestId: string, optionId: string) => Promise<unknown>;
}

/** Raw agent JSON, shown as text and never interpreted. */
function RawJson({ label, json }: { label: string; json: string }) {
  if (json === "") return null;
  let text = json;
  try {
    text = JSON.stringify(JSON.parse(json), null, 2);
  } catch {
    // Not JSON: show the text as it is.
  }
  return (
    <div className="raw-json">
      <p className="raw-json-label">{label}</p>
      <pre className="code-block">
        <code>{text}</code>
      </pre>
    </div>
  );
}

/** A section that starts collapsed. */
function Disclosure({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <Collapsible className="disclosure">
      <CollapsibleTrigger size={ButtonSize.Small}>{label}</CollapsibleTrigger>
      <CollapsiblePanel className="disclosure-panel">
        {children}
      </CollapsiblePanel>
    </Collapsible>
  );
}

export function ContentBlockView({ block }: { block: ContentBlock }) {
  switch (block.block.case) {
    case "text":
      return <Markdown text={block.block.value} />;
    case "resourceLink": {
      const link = block.block.value;
      const name = link.title || link.name || link.uri;
      const href = safeHref(link.uri);
      return (
        <p className="resource-link">
          {href === undefined ? (
            <code>{name}</code>
          ) : (
            <a href={href} target="_blank" rel="noopener noreferrer">
              {name}
            </a>
          )}
        </p>
      );
    }
    case "unsupported":
      return (
        <Disclosure label="Content that the console cannot show">
          <RawJson label="Original content" json={block.block.value.rawJson} />
        </Disclosure>
      );
    case undefined:
      return null;
  }
}

function DiffView({ diff }: { diff: Diff }) {
  const lines = diffLines(diff.oldText, diff.newText);
  return (
    <figure className="diff">
      <figcaption>
        <code>{diff.path}</code>
        {diff.oldText === undefined ? " (new file)" : null}
      </figcaption>
      <pre
        className="code-block diff-lines"
        aria-label={`Changes in ${diff.path}`}
      >
        {lines.map((line, index) => (
          <span key={index} className={`diff-line diff-${line.kind}`}>
            {line.kind === DiffLineKind.Added
              ? "+ "
              : line.kind === DiffLineKind.Removed
                ? "- "
                : "  "}
            {line.text}
            {"\n"}
          </span>
        ))}
      </pre>
    </figure>
  );
}

function ToolCallContentView({ content }: { content: ToolCallContent }) {
  switch (content.content.case) {
    case "block":
      return <ContentBlockView block={content.content.value} />;
    case "diff":
      return <DiffView diff={content.content.value} />;
    case "terminal":
      return (
        <p className="muted">
          Terminal <code>{content.content.value.terminalId}</code>
        </p>
      );
    case undefined:
      return null;
  }
}

function toolStatusTone(status: ToolCallStatus): BadgeVariant {
  if (status === ToolCallStatus.FAILED) return BadgeVariant.Danger;
  if (status === ToolCallStatus.IN_PROGRESS) return BadgeVariant.Accent;
  return BadgeVariant.Neutral;
}

function ToolCallView({ item }: { item: ToolCallItem }) {
  const diffs = item.content.filter((c) => c.content.case === "diff");
  const others = item.content.filter((c) => c.content.case !== "diff");
  const hasDetails =
    others.length > 0 ||
    item.locations.length > 0 ||
    item.rawInput !== "" ||
    item.rawOutput !== "";
  return (
    <article
      className="chat-tool"
      aria-label={`Tool call: ${item.title || "Untitled"}`}
    >
      <header className="chat-tool-header">
        <span className="chat-tool-title">{item.title || "Tool call"}</span>
        <span className="muted">{labels.toolKind[item.toolKind]}</span>
        <StateBadge
          text={labels.toolCallStatus[item.status] ?? "Unknown"}
          tone={toolStatusTone(item.status)}
        />
      </header>
      {diffs.map((c, index) => (
        <ToolCallContentView key={index} content={c} />
      ))}
      {hasDetails ? (
        <Disclosure label="Input and output">
          {item.locations.length > 0 ? (
            <ul className="chat-tool-locations">
              {item.locations.map((l, index) => (
                <li key={index}>
                  <code>
                    {l.path}
                    {l.line === undefined ? "" : `:${l.line}`}
                  </code>
                </li>
              ))}
            </ul>
          ) : null}
          {others.map((c, index) => (
            <ToolCallContentView key={index} content={c} />
          ))}
          <RawJson label="Input" json={item.rawInput} />
          <RawJson label="Output" json={item.rawOutput} />
        </Disclosure>
      ) : null}
    </article>
  );
}

function optionVariant(option: PermissionOption): ButtonVariant {
  return option.kind === PermissionOptionKind.ALLOW_ONCE ||
    option.kind === PermissionOptionKind.ALLOW_ALWAYS
    ? ButtonVariant.Primary
    : ButtonVariant.Secondary;
}

function PermissionView({
  item,
  actions,
}: {
  item: PermissionItem;
  actions: PermissionActions;
}) {
  const subject = item.toolTitle || "a tool call";
  const response = item.response;
  let outcome: ReactNode = null;
  if (response !== undefined) {
    const option = item.options.find((o) => o.optionId === response.optionId);
    outcome =
      response.outcome === PermissionOutcome.CANCELLED ? (
        <p className="muted">The request was cancelled.</p>
      ) : (
        <p>
          {response.autoApproved ? (
            <StateBadge text="Auto-approved" tone={BadgeVariant.Accent} />
          ) : null}{" "}
          Answer: {option?.name ?? response.optionId}
        </p>
      );
  }
  return (
    <article
      className={
        response === undefined
          ? "chat-permission chat-permission-pending"
          : "chat-permission"
      }
      aria-label={`Permission request for ${subject}`}
    >
      <p className="chat-permission-title">
        {response?.autoApproved
          ? `Auto-approved permission for ${subject}`
          : `The agent asks for permission for ${subject}`}
      </p>
      {response === undefined ? (
        actions.answer === undefined ? (
          <p className="muted">Waiting for an answer.</p>
        ) : (
          <div className="chat-permission-options">
            {item.options.map((option) => (
              <Button
                key={option.optionId}
                size={ButtonSize.Small}
                variant={optionVariant(option)}
                onClick={() =>
                  actions.answer!(item.permissionRequestId, option.optionId)
                }
              >
                {option.name}
              </Button>
            ))}
          </div>
        )
      ) : (
        outcome
      )}
    </article>
  );
}

/** One item of the chat view. */
export function ChatItemView({
  item,
  actions,
}: {
  item: ChatItem;
  actions: PermissionActions;
}) {
  switch (item.kind) {
    case ChatItemKind.UserMessage:
      return (
        <article className="chat-message chat-user" aria-label="Your message">
          {item.blocks.map((b, i) => (
            <ContentBlockView key={i} block={b} />
          ))}
        </article>
      );
    case ChatItemKind.AgentMessage:
      return (
        <article className="chat-message chat-agent" aria-label="Agent message">
          {item.blocks.map((b, i) => (
            <ContentBlockView key={i} block={b} />
          ))}
        </article>
      );
    case ChatItemKind.AgentThought:
      return (
        <div className="chat-thought">
          <Disclosure label="Thought">
            {item.blocks.map((b, i) => (
              <ContentBlockView key={i} block={b} />
            ))}
          </Disclosure>
        </div>
      );
    case ChatItemKind.ToolCall:
      return <ToolCallView item={item} />;
    case ChatItemKind.Plan:
      return (
        <article className="chat-plan" aria-label="Plan">
          <p className="chat-plan-title">Plan</p>
          <ol>
            {item.entries.map((entry, index) => (
              <li key={index}>
                {entry.content}{" "}
                <span className="muted">
                  · {labels.planStatus[entry.status]} ·{" "}
                  {labels.planPriority[entry.priority]} priority
                </span>
              </li>
            ))}
          </ol>
        </article>
      );
    case ChatItemKind.Permission:
      return <PermissionView item={item} actions={actions} />;
    case ChatItemKind.TurnEnded:
      return (
        <p className="chat-divider" role="note">
          {labels.stopReason[item.stopReason] ?? "Turn ended"}
        </p>
      );
    case ChatItemKind.ModeChanged:
      return (
        <p className="chat-divider" role="note">
          Mode: {item.modeName || "Agent default"} ·{" "}
          {labels.permissionMode[item.permissionMode]}
        </p>
      );
    case ChatItemKind.BridgeError:
      return (
        <div className="notice notice-error chat-error" role="note">
          <p>
            <strong>{labels.bridgeError[item.errorKind]}</strong>
            {item.message === "" ? null : `: ${item.message}`}
          </p>
        </div>
      );
    case ChatItemKind.InputRejected:
      return (
        <p className="chat-divider" role="note">
          The agent did not accept an input:{" "}
          {labels.inputRejection[item.reason]?.toLowerCase()}
        </p>
      );
    case ChatItemKind.AgentExited:
      return (
        <p className="chat-divider" role="note">
          {item.signal !== undefined
            ? `The agent exited with the signal ${item.signal}.`
            : `The agent exited with the code ${item.exitCode ?? 0}.`}
        </p>
      );
    case ChatItemKind.Unknown:
      return (
        <div className="chat-unknown">
          <Disclosure
            label={`Agent update: ${item.sessionUpdate || "unknown"}`}
          >
            <RawJson label="Original update" json={item.rawJson} />
          </Disclosure>
        </div>
      );
  }
}
