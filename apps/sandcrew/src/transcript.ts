// SPDX-License-Identifier: Apache-2.0
import {
  AgentActivity,
  BridgeErrorKind,
  InputRejectionReason,
  StopReason,
  ToolCallStatus,
  ToolKind,
} from "@delino/sandcrew-protos/v1/bridge_pb";
import type {
  ConfigOption,
  ContentBlock,
  PermissionOption,
  PermissionResponse,
  PlanEntry,
  SessionMode,
  ToolCallContent,
  ToolCallLocation,
} from "@delino/sandcrew-protos/v1/bridge_pb";
import { PermissionMode } from "@delino/sandcrew-protos/v1/common_pb";
import type { TranscriptEvent } from "@delino/sandcrew-protos/v1/session_pb";

/** The kinds of chat items that the chat view renders. */
export enum ChatItemKind {
  UserMessage = "user-message",
  AgentMessage = "agent-message",
  AgentThought = "agent-thought",
  ToolCall = "tool-call",
  Plan = "plan",
  Permission = "permission",
  TurnEnded = "turn-ended",
  ModeChanged = "mode-changed",
  BridgeError = "bridge-error",
  InputRejected = "input-rejected",
  AgentExited = "agent-exited",
  Unknown = "unknown",
}

/** A message, a thought: the content blocks of consecutive chunks. */
export interface ContentItem {
  kind:
    | ChatItemKind.UserMessage
    | ChatItemKind.AgentMessage
    | ChatItemKind.AgentThought;
  key: string;
  /** Adjacent text blocks are joined into one. */
  blocks: ContentBlock[];
}

export interface ToolCallItem {
  kind: ChatItemKind.ToolCall;
  key: string;
  toolCallId: string;
  title: string;
  toolKind: ToolKind;
  status: ToolCallStatus;
  content: ToolCallContent[];
  locations: ToolCallLocation[];
  rawInput: string;
  rawOutput: string;
}

export interface PlanItem {
  kind: ChatItemKind.Plan;
  key: string;
  entries: PlanEntry[];
}

export interface PermissionItem {
  kind: ChatItemKind.Permission;
  key: string;
  permissionRequestId: string;
  toolCallId: string;
  /** The title of the tool call, when the transcript has it. */
  toolTitle: string;
  options: PermissionOption[];
  /** Unset while the request waits for an answer. */
  response: PermissionResponse | undefined;
}

export interface TurnEndedItem {
  kind: ChatItemKind.TurnEnded;
  key: string;
  stopReason: StopReason;
}

export interface ModeChangedItem {
  kind: ChatItemKind.ModeChanged;
  key: string;
  modeName: string;
  permissionMode: PermissionMode;
}

export interface BridgeErrorItem {
  kind: ChatItemKind.BridgeError;
  key: string;
  errorKind: BridgeErrorKind;
  message: string;
}

export interface InputRejectedItem {
  kind: ChatItemKind.InputRejected;
  key: string;
  reason: InputRejectionReason;
}

export interface AgentExitedItem {
  kind: ChatItemKind.AgentExited;
  key: string;
  exitCode: number | undefined;
  signal: string | undefined;
}

export interface UnknownItem {
  kind: ChatItemKind.Unknown;
  key: string;
  sessionUpdate: string;
  rawJson: string;
}

export type ChatItem =
  | ContentItem
  | ToolCallItem
  | PlanItem
  | PermissionItem
  | TurnEndedItem
  | ModeChangedItem
  | BridgeErrorItem
  | InputRejectedItem
  | AgentExitedItem
  | UnknownItem;

/** The chat items and the agent state that the transcript describes. */
export interface Transcript {
  items: ChatItem[];
  /** The latest activity that the bridge reported. */
  activity: AgentActivity;
  currentModeId: string;
  availableModes: SessionMode[];
  configOptions: ConfigOption[];
  /** Unset until the bridge reports a permission mode. */
  permissionMode: PermissionMode | undefined;
  /** IDs of permission requests without a response. */
  pendingPermissionIds: string[];
  /** The bridge could not install the agent (download or checksum). */
  agentInstallFailed: boolean;
  /** The greatest sequence of the events. */
  lastSequence: bigint;
}

const INSTALL_FAILURES: ReadonlySet<BridgeErrorKind> = new Set([
  BridgeErrorKind.AGENT_INSTALL_FAILED,
  BridgeErrorKind.CHECKSUM_MISMATCH,
]);

/** Appends a block, joining it to the previous text block. */
function appendBlock(blocks: ContentBlock[], block: ContentBlock | undefined) {
  if (block === undefined) return;
  const last = blocks.at(-1);
  if (
    block.block.case === "text" &&
    last !== undefined &&
    last.block.case === "text"
  ) {
    blocks[blocks.length - 1] = {
      ...last,
      block: { case: "text", value: last.block.value + block.block.value },
    };
    return;
  }
  blocks.push(block);
}

/**
 * Builds the chat items from transcript events. Events are applied in
 * sequence order; a repeated sequence is ignored, so a reconnect that sends
 * an event again does not duplicate it.
 */
export function buildTranscript(
  events: readonly TranscriptEvent[],
): Transcript {
  const sorted = [...events].sort((a, b) =>
    a.sequence < b.sequence ? -1 : a.sequence > b.sequence ? 1 : 0,
  );
  const items: ChatItem[] = [];
  const toolCalls = new Map<string, ToolCallItem>();
  const permissions = new Map<string, PermissionItem>();
  const result: Transcript = {
    items,
    activity: AgentActivity.UNSPECIFIED,
    currentModeId: "",
    availableModes: [],
    configOptions: [],
    permissionMode: undefined,
    pendingPermissionIds: [],
    agentInstallFailed: false,
    lastSequence: 0n,
  };
  let previous: bigint | undefined;
  for (const event of sorted) {
    if (event.sequence === previous) continue;
    previous = event.sequence;
    result.lastSequence = event.sequence;
    const key = `e${event.sequence}`;
    const e = event.event;
    switch (e.case) {
      case "userMessageChunk":
      case "agentMessageChunk":
      case "agentThoughtChunk": {
        const kind =
          e.case === "userMessageChunk"
            ? ChatItemKind.UserMessage
            : e.case === "agentMessageChunk"
              ? ChatItemKind.AgentMessage
              : ChatItemKind.AgentThought;
        const last = items.at(-1);
        if (last !== undefined && last.kind === kind) {
          appendBlock(last.blocks, e.value.content);
        } else {
          const item: ContentItem = { kind, key, blocks: [] };
          appendBlock(item.blocks, e.value.content);
          items.push(item);
        }
        break;
      }
      case "toolCall": {
        const call = e.value;
        const existing = toolCalls.get(call.toolCallId);
        const item: ToolCallItem = {
          kind: ChatItemKind.ToolCall,
          key: existing?.key ?? key,
          toolCallId: call.toolCallId,
          title: call.title,
          toolKind: call.kind,
          status: call.status,
          content: call.content,
          locations: call.locations,
          rawInput: call.rawInput,
          rawOutput: existing?.rawOutput ?? "",
        };
        if (existing === undefined) {
          items.push(item);
        } else {
          items[items.indexOf(existing)] = item;
        }
        toolCalls.set(call.toolCallId, item);
        break;
      }
      case "toolCallUpdate": {
        const update = e.value;
        let item = toolCalls.get(update.toolCallId);
        if (item === undefined) {
          item = {
            kind: ChatItemKind.ToolCall,
            key,
            toolCallId: update.toolCallId,
            title: "",
            toolKind: ToolKind.UNSPECIFIED,
            status: ToolCallStatus.UNSPECIFIED,
            content: [],
            locations: [],
            rawInput: "",
            rawOutput: "",
          };
          items.push(item);
          toolCalls.set(update.toolCallId, item);
        }
        const next: ToolCallItem = {
          ...item,
          title: update.title ?? item.title,
          toolKind: update.kind ?? item.toolKind,
          status: update.status ?? item.status,
          content: update.content?.items ?? item.content,
          locations: update.locations?.items ?? item.locations,
          rawInput: update.rawInput ?? item.rawInput,
          rawOutput: update.rawOutput ?? item.rawOutput,
        };
        items[items.indexOf(item)] = next;
        toolCalls.set(update.toolCallId, next);
        break;
      }
      case "plan": {
        // A plan replaces the previous plan: show only the latest one.
        const old = items.findIndex((i) => i.kind === ChatItemKind.Plan);
        if (old !== -1) items.splice(old, 1);
        items.push({ kind: ChatItemKind.Plan, key, entries: e.value.entries });
        break;
      }
      case "permissionRequest": {
        const request = e.value;
        const item: PermissionItem = {
          kind: ChatItemKind.Permission,
          key,
          permissionRequestId: request.permissionRequestId,
          toolCallId: request.toolCallId,
          toolTitle: toolCalls.get(request.toolCallId)?.title ?? "",
          options: request.options,
          response: undefined,
        };
        items.push(item);
        permissions.set(request.permissionRequestId, item);
        break;
      }
      case "permissionResponse": {
        const response = e.value;
        const item = permissions.get(response.permissionRequestId);
        if (item === undefined) {
          // An answer without its request still records the decision.
          const orphan: PermissionItem = {
            kind: ChatItemKind.Permission,
            key,
            permissionRequestId: response.permissionRequestId,
            toolCallId: "",
            toolTitle: "",
            options: [],
            response,
          };
          items.push(orphan);
          permissions.set(response.permissionRequestId, orphan);
        } else {
          const next = { ...item, response };
          items[items.indexOf(item)] = next;
          permissions.set(response.permissionRequestId, next);
        }
        break;
      }
      case "turnEnded":
        items.push({
          kind: ChatItemKind.TurnEnded,
          key,
          stopReason: e.value.stopReason,
        });
        break;
      case "modeChanged": {
        const changed = e.value;
        const modeChanged =
          result.currentModeId !== "" &&
          changed.currentModeId !== result.currentModeId;
        const permissionChanged =
          result.permissionMode !== undefined &&
          changed.permissionMode !== result.permissionMode;
        result.currentModeId = changed.currentModeId;
        result.availableModes = changed.availableModes;
        if (changed.permissionMode !== PermissionMode.UNSPECIFIED) {
          result.permissionMode = changed.permissionMode;
        }
        if (modeChanged || permissionChanged) {
          items.push({
            kind: ChatItemKind.ModeChanged,
            key,
            modeName:
              changed.availableModes.find(
                (m) => m.modeId === changed.currentModeId,
              )?.name ?? changed.currentModeId,
            permissionMode: changed.permissionMode,
          });
        }
        break;
      }
      case "configOptions":
        result.configOptions = e.value.options;
        break;
      case "sessionState":
        result.activity = e.value.state;
        break;
      case "bridgeError":
        if (INSTALL_FAILURES.has(e.value.kind)) {
          result.agentInstallFailed = true;
        }
        items.push({
          kind: ChatItemKind.BridgeError,
          key,
          errorKind: e.value.kind,
          message: e.value.message,
        });
        break;
      case "inputRejected":
        items.push({
          kind: ChatItemKind.InputRejected,
          key,
          reason: e.value.reason,
        });
        break;
      case "agentExited":
        items.push({
          kind: ChatItemKind.AgentExited,
          key,
          exitCode:
            e.value.result.case === "exitCode"
              ? e.value.result.value
              : undefined,
          signal:
            e.value.result.case === "signal" ? e.value.result.value : undefined,
        });
        break;
      case "unknownUpdate":
        items.push({
          kind: ChatItemKind.Unknown,
          key,
          sessionUpdate: e.value.sessionUpdate,
          rawJson: e.value.rawJson,
        });
        break;
      case "hello":
      case "agentReady":
      case "inputAccepted":
      case undefined:
        // hello, agent_ready and input_accepted carry no chat content. An unknown case
        // of a newer protocol minor keeps its sequence and shows nothing.
        break;
    }
  }
  result.pendingPermissionIds = [...permissions.values()]
    .filter((p) => p.response === undefined)
    .map((p) => p.permissionRequestId);
  return result;
}

/** Whether the agent works on a turn that the user can stop. */
export function isTurnRunning(transcript: Transcript): boolean {
  return (
    transcript.activity === AgentActivity.WORKING ||
    transcript.activity === AgentActivity.WAITING_FOR_PERMISSION
  );
}

/**
 * Adds events to a list ordered by sequence, ignoring sequences that it
 * already holds. Returns the same array when nothing changed.
 */
export function mergeEvents(
  current: readonly TranscriptEvent[],
  added: readonly TranscriptEvent[],
): readonly TranscriptEvent[] {
  if (added.length === 0) return current;
  const known = new Set(current.map((e) => e.sequence));
  const fresh = added.filter((e) => {
    if (known.has(e.sequence)) return false;
    known.add(e.sequence);
    return true;
  });
  if (fresh.length === 0) return current;
  return [...current, ...fresh].sort((a, b) =>
    a.sequence < b.sequence ? -1 : a.sequence > b.sequence ? 1 : 0,
  );
}
