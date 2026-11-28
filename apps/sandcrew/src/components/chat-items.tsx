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
import { useI18n } from "../i18n.ts";
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
  const { t } = useI18n();
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
        <Disclosure label={t("chat.unsupportedContent")}>
          <RawJson
            label={t("chat.originalContent")}
            json={block.block.value.rawJson}
          />
        </Disclosure>
      );
    case undefined:
      return null;
  }
}

function DiffView({ diff }: { diff: Diff }) {
  const { t } = useI18n();
  const lines = diffLines(diff.oldText, diff.newText);
  return (
    <figure className="diff">
      <figcaption>
        <code>{diff.path}</code>
        {diff.oldText === undefined ? ` ${t("chat.diff.newFile")}` : null}
      </figcaption>
      <pre
        className="code-block diff-lines"
        aria-label={t("chat.diff.changesIn", { path: diff.path })}
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
  const { rich } = useI18n();
  switch (content.content.case) {
    case "block":
      return <ContentBlockView block={content.content.value} />;
    case "diff":
      return <DiffView diff={content.content.value} />;
    case "terminal":
      return (
        <p className="muted">
          {rich("chat.terminal", {
            id: <code>{content.content.value.terminalId}</code>,
          })}
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
  const { t } = useI18n();
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
      aria-label={t("chat.tool.label", {
        title: item.title || t("chat.tool.untitled"),
      })}
    >
      <header className="chat-tool-header">
        <span className="chat-tool-title">
          {item.title || t("chat.tool.title")}
        </span>
        <span className="muted">
          {t(labels.toolKind[item.toolKind] ?? "enum.unknown")}
        </span>
        <StateBadge
          text={t(labels.toolCallStatus[item.status] ?? "enum.unknown")}
          tone={toolStatusTone(item.status)}
        />
      </header>
      {diffs.map((c, index) => (
        <ToolCallContentView key={index} content={c} />
      ))}
      {hasDetails ? (
        <Disclosure label={t("chat.tool.details")}>
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
          <RawJson label={t("chat.tool.input")} json={item.rawInput} />
          <RawJson label={t("chat.tool.output")} json={item.rawOutput} />
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
  const { t } = useI18n();
  const subject = item.toolTitle || t("chat.permission.aToolCall");
  const response = item.response;
  let outcome: ReactNode = null;
  if (response !== undefined) {
    const option = item.options.find((o) => o.optionId === response.optionId);
    outcome =
      response.outcome === PermissionOutcome.CANCELLED ? (
        <p className="muted">{t("chat.permission.cancelled")}</p>
      ) : (
        <p>
          {response.autoApproved ? (
            <StateBadge
              text={t("chat.permission.autoApprovedBadge")}
              tone={BadgeVariant.Accent}
            />
          ) : null}{" "}
          {t("chat.permission.answer", {
            option: option?.name ?? response.optionId,
          })}
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
      aria-label={t("chat.permission.label", { subject })}
    >
      <p className="chat-permission-title">
        {response?.autoApproved
          ? t("chat.permission.autoApproved", { subject })
          : t("chat.permission.asks", { subject })}
      </p>
      {response === undefined ? (
        actions.answer === undefined ? (
          <p className="muted">{t("chat.permission.waiting")}</p>
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
  const { t } = useI18n();
  switch (item.kind) {
    case ChatItemKind.UserMessage:
      return (
        <article
          className="chat-message chat-user"
          aria-label={t("chat.userMessage")}
        >
          {item.blocks.map((b, i) => (
            <ContentBlockView key={i} block={b} />
          ))}
        </article>
      );
    case ChatItemKind.AgentMessage:
      return (
        <article
          className="chat-message chat-agent"
          aria-label={t("chat.agentMessage")}
        >
          {item.blocks.map((b, i) => (
            <ContentBlockView key={i} block={b} />
          ))}
        </article>
      );
    case ChatItemKind.AgentThought:
      return (
        <div className="chat-thought">
          <Disclosure label={t("chat.thought")}>
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
        <article className="chat-plan" aria-label={t("chat.plan")}>
          <p className="chat-plan-title">{t("chat.plan")}</p>
          <ol>
            {item.entries.map((entry, index) => (
              <li key={index}>
                {entry.content}{" "}
                <span className="muted">
                  {t("chat.planEntry", {
                    status: t(
                      labels.planStatus[entry.status] ?? "enum.unknown",
                    ),
                    priority: t(
                      labels.planPriority[entry.priority] ?? "enum.unknown",
                    ),
                  })}
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
          {t(labels.stopReason[item.stopReason] ?? "enum.stopReason.endTurn")}
        </p>
      );
    case ChatItemKind.ModeChanged:
      return (
        <p className="chat-divider" role="note">
          {t("chat.modeChanged", {
            mode: item.modeName || t("chat.agentDefaultMode"),
            permissions: t(
              labels.permissionMode[item.permissionMode] ?? "enum.unknown",
            ),
          })}
        </p>
      );
    case ChatItemKind.BridgeError:
      return (
        <div className="notice notice-error chat-error" role="note">
          <p>
            <strong>
              {t(labels.bridgeError[item.errorKind] ?? "enum.unknown")}
            </strong>
            {item.message === "" ? null : `: ${item.message}`}
          </p>
        </div>
      );
    case ChatItemKind.InputRejected:
      return (
        <p className="chat-divider" role="note">
          {t("chat.inputRejected", {
            reason: t(
              labels.inputRejection[item.reason] ??
                "enum.inputRejection.unspecified",
            ),
          })}
        </p>
      );
    case ChatItemKind.AgentExited:
      return (
        <p className="chat-divider" role="note">
          {item.signal !== undefined
            ? t("chat.agentExitedSignal", { signal: item.signal })
            : t("chat.agentExitedCode", { code: item.exitCode ?? 0 })}
        </p>
      );
    case ChatItemKind.Unknown:
      return (
        <div className="chat-unknown">
          <Disclosure
            label={t("chat.unknownUpdate", {
              update: item.sessionUpdate || t("chat.unknownUpdateName"),
            })}
          >
            <RawJson label={t("chat.originalUpdate")} json={item.rawJson} />
          </Disclosure>
        </div>
      );
  }
}
