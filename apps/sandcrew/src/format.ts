// SPDX-License-Identifier: Apache-2.0
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BridgeErrorKind,
  InputRejectionReason,
  PlanEntryPriority,
  PlanEntryStatus,
  StopReason,
  ToolCallStatus,
  ToolKind,
} from "@delino/sandcrew-protos/v1/bridge_pb";
import {
  AgentVendor,
  GuestOs,
  PermissionMode,
  ProfileIneligibility,
  Role,
  SessionEndReason,
  SessionState,
  VendorCredentialKind,
} from "@delino/sandcrew-protos/v1/common_pb";
import type { Localizer, MessageKey, Translate } from "./i18n.ts";

const UNKNOWN = "enum.unknown";

/**
 * Catalog keys of the enum values that the console shows, by value. Render
 * a label with `t(labels.<enum>[value])`.
 */
export const labels = {
  role: {
    [Role.UNSPECIFIED]: UNKNOWN,
    [Role.OWNER]: "enum.role.owner",
    [Role.ADMIN]: "enum.role.admin",
    [Role.MEMBER]: "enum.role.member",
  },
  sessionState: {
    [SessionState.UNSPECIFIED]: UNKNOWN,
    [SessionState.QUEUED]: "enum.sessionState.queued",
    [SessionState.STARTING]: "enum.sessionState.starting",
    [SessionState.RUNNING]: "enum.sessionState.running",
    [SessionState.WAITING_FOR_INPUT]: "enum.sessionState.waitingForInput",
    [SessionState.PAUSING]: "enum.sessionState.pausing",
    [SessionState.PAUSED]: "enum.sessionState.paused",
    [SessionState.RESUME_QUEUED]: "enum.sessionState.resumeQueued",
    [SessionState.RESUMING]: "enum.sessionState.resuming",
    [SessionState.DISCONNECTED]: "enum.sessionState.disconnected",
    [SessionState.ENDED]: "enum.sessionState.ended",
  },
  endReason: {
    [SessionEndReason.UNSPECIFIED]: UNKNOWN,
    [SessionEndReason.START_FAILED]: "enum.endReason.startFailed",
    [SessionEndReason.QUEUE_EXPIRED]: "enum.endReason.queueExpired",
    [SessionEndReason.AGENT_EXITED]: "enum.endReason.agentExited",
    [SessionEndReason.DEVICE_LOST]: "enum.endReason.deviceLost",
    [SessionEndReason.RESTORE_FAILED]: "enum.endReason.restoreFailed",
    [SessionEndReason.PAUSED_RETENTION_EXPIRED]:
      "enum.endReason.pausedRetentionExpired",
    [SessionEndReason.BRIDGE_UNSUPPORTED]: "enum.endReason.bridgeUnsupported",
    [SessionEndReason.EXECUTOR_ENDED]: "enum.endReason.executorEnded",
  },
  permissionMode: {
    [PermissionMode.UNSPECIFIED]: UNKNOWN,
    [PermissionMode.AUTO_APPROVE]: "enum.permissionMode.autoApprove",
    [PermissionMode.ASK]: "enum.permissionMode.ask",
  },
  credentialKind: {
    [VendorCredentialKind.UNSPECIFIED]: UNKNOWN,
    [VendorCredentialKind.API_KEY]: "enum.credentialKind.apiKey",
    [VendorCredentialKind.CLAUDE_OAUTH_TOKEN]:
      "enum.credentialKind.claudeOauthToken",
    [VendorCredentialKind.CODEX_AUTH_JSON]: "enum.credentialKind.codexAuthJson",
  },
  ineligibility: {
    [ProfileIneligibility.UNSPECIFIED]: UNKNOWN,
    [ProfileIneligibility.HAS_MAXIMUM_LIFETIME]:
      "enum.ineligibility.hasMaximumLifetime",
    [ProfileIneligibility.NETWORK_NOT_OUTBOUND]:
      "enum.ineligibility.networkNotOutbound",
    [ProfileIneligibility.GUEST_OS_UNSUPPORTED]:
      "enum.ineligibility.guestOsUnsupported",
  },
  toolKind: {
    [ToolKind.UNSPECIFIED]: UNKNOWN,
    [ToolKind.READ]: "enum.toolKind.read",
    [ToolKind.EDIT]: "enum.toolKind.edit",
    [ToolKind.DELETE]: "enum.toolKind.delete",
    [ToolKind.MOVE]: "enum.toolKind.move",
    [ToolKind.SEARCH]: "enum.toolKind.search",
    [ToolKind.EXECUTE]: "enum.toolKind.execute",
    [ToolKind.THINK]: "enum.toolKind.think",
    [ToolKind.FETCH]: "enum.toolKind.fetch",
    [ToolKind.SWITCH_MODE]: "enum.toolKind.switchMode",
    [ToolKind.OTHER]: "enum.toolKind.other",
  },
  toolCallStatus: {
    [ToolCallStatus.UNSPECIFIED]: UNKNOWN,
    [ToolCallStatus.PENDING]: "enum.toolCallStatus.pending",
    [ToolCallStatus.IN_PROGRESS]: "enum.toolCallStatus.inProgress",
    [ToolCallStatus.COMPLETED]: "enum.toolCallStatus.completed",
    [ToolCallStatus.FAILED]: "enum.toolCallStatus.failed",
  },
  planPriority: {
    [PlanEntryPriority.UNSPECIFIED]: UNKNOWN,
    [PlanEntryPriority.HIGH]: "enum.planPriority.high",
    [PlanEntryPriority.MEDIUM]: "enum.planPriority.medium",
    [PlanEntryPriority.LOW]: "enum.planPriority.low",
  },
  planStatus: {
    [PlanEntryStatus.UNSPECIFIED]: UNKNOWN,
    [PlanEntryStatus.PENDING]: "enum.planStatus.pending",
    [PlanEntryStatus.IN_PROGRESS]: "enum.planStatus.inProgress",
    [PlanEntryStatus.COMPLETED]: "enum.planStatus.completed",
  },
  stopReason: {
    [StopReason.UNSPECIFIED]: UNKNOWN,
    [StopReason.END_TURN]: "enum.stopReason.endTurn",
    [StopReason.MAX_TOKENS]: "enum.stopReason.maxTokens",
    [StopReason.MAX_TURN_REQUESTS]: "enum.stopReason.maxTurnRequests",
    [StopReason.REFUSAL]: "enum.stopReason.refusal",
    [StopReason.CANCELLED]: "enum.stopReason.cancelled",
  },
  bridgeError: {
    [BridgeErrorKind.UNSPECIFIED]: UNKNOWN,
    [BridgeErrorKind.AGENT_INSTALL_FAILED]:
      "enum.bridgeError.agentInstallFailed",
    [BridgeErrorKind.CHECKSUM_MISMATCH]: "enum.bridgeError.checksumMismatch",
    [BridgeErrorKind.AGENT_START_FAILED]: "enum.bridgeError.agentStartFailed",
    [BridgeErrorKind.CREDENTIAL_MISSING]: "enum.bridgeError.credentialMissing",
    [BridgeErrorKind.ACP_PROTOCOL_ERROR]: "enum.bridgeError.acpProtocolError",
    [BridgeErrorKind.INPUT_INVALID]: "enum.bridgeError.inputInvalid",
    [BridgeErrorKind.OPTION_REJECTED]: "enum.bridgeError.optionRejected",
    [BridgeErrorKind.EVENT_LOG_WRITE_FAILED]:
      "enum.bridgeError.eventLogWriteFailed",
    [BridgeErrorKind.CREDENTIAL_WATCH_FAILED]:
      "enum.bridgeError.credentialWatchFailed",
  },
  inputRejection: {
    [InputRejectionReason.UNSPECIFIED]: "enum.inputRejection.unspecified",
    [InputRejectionReason.MALFORMED]: "enum.inputRejection.malformed",
    [InputRejectionReason.UNKNOWN_PERMISSION_REQUEST]:
      "enum.inputRejection.unknownPermissionRequest",
    [InputRejectionReason.UNKNOWN_OPTION]: "enum.inputRejection.unknownOption",
    [InputRejectionReason.NO_ACTIVE_TURN]: "enum.inputRejection.noActiveTurn",
    [InputRejectionReason.UNSUPPORTED]: "enum.inputRejection.unsupported",
  },
} satisfies Record<string, Record<number, MessageKey>>;

/** Product names of the agent vendors; they stay untranslated. */
const VENDOR_NAMES: Readonly<Partial<Record<AgentVendor, string>>> = {
  [AgentVendor.CLAUDE_CODE]: "Claude Code",
  [AgentVendor.OPENAI_CODEX]: "OpenAI Codex",
};

/** Product names of the guest operating systems; they stay untranslated. */
const GUEST_OS_NAMES: Readonly<Partial<Record<GuestOs, string>>> = {
  [GuestOs.LINUX]: "Linux",
  [GuestOs.MACOS]: "macOS",
  [GuestOs.WINDOWS]: "Windows",
};

/** The product name of a vendor, or "Unknown". */
export function vendorName(vendor: AgentVendor, t: Translate): string {
  return VENDOR_NAMES[vendor] ?? t(UNKNOWN);
}

/** The product name of a guest OS, or "Unknown". */
export function guestOsName(os: GuestOs, t: Translate): string {
  return GUEST_OS_NAMES[os] ?? t(UNKNOWN);
}

/** The guest OS and resources of a runner profile, for example "Linux · 4 vCPU · …". */
export function profileSummary(
  profile: {
    guestOs: GuestOs;
    vcpuCount: number | bigint;
    memoryMib: number | bigint;
    diskGib: number | bigint;
  },
  { lang, t }: Localizer,
): string {
  const n = new Intl.NumberFormat(lang).format;
  return t("profile.summary", {
    os: guestOsName(profile.guestOs, t),
    vcpu: n(profile.vcpuCount),
    memory: n(profile.memoryMib),
    disk: n(profile.diskGib),
  });
}

/** The vendors that the console offers, in display order. */
export const VENDORS: readonly AgentVendor[] = [
  AgentVendor.CLAUDE_CODE,
  AgentVendor.OPENAI_CODEX,
];

/** The credential kinds that each vendor accepts. */
export const VENDOR_CREDENTIAL_KINDS: Readonly<
  Record<AgentVendor, readonly VendorCredentialKind[]>
> = {
  [AgentVendor.UNSPECIFIED]: [],
  [AgentVendor.CLAUDE_CODE]: [
    VendorCredentialKind.API_KEY,
    VendorCredentialKind.CLAUDE_OAUTH_TOKEN,
  ],
  [AgentVendor.OPENAI_CODEX]: [
    VendorCredentialKind.API_KEY,
    VendorCredentialKind.CODEX_AUTH_JSON,
  ],
};

/** Compares timestamps at nanosecond precision, treating a missing time as the epoch. */
export function compareTimestamps(
  a: Timestamp | undefined,
  b: Timestamp | undefined,
): number {
  return (
    Number((a?.seconds ?? 0n) - (b?.seconds ?? 0n)) ||
    (a?.nanos ?? 0) - (b?.nanos ?? 0)
  );
}

export function timestampMs(ts: Timestamp | undefined): number | undefined {
  if (ts === undefined) return undefined;
  return Number(ts.seconds) * 1000 + Math.floor(ts.nanos / 1e6);
}

/** Formats a time for the selected language. */
export function formatTime(ts: Timestamp | undefined, lang: string): string {
  const ms = timestampMs(ts);
  if (ms === undefined) return "—";
  return new Date(ms).toLocaleString(lang, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

export function durationMs(duration: Duration | undefined): number | undefined {
  if (duration === undefined) return undefined;
  return Number(duration.seconds) * 1000 + Math.floor(duration.nanos / 1e6);
}

/**
 * The text of a failed request. Server messages of precondition, argument,
 * conflict and limit errors are shown as received.
 */
export function errorMessage(
  error: unknown,
  t: Translate,
  fallback = t("errors.generic"),
): string {
  if (!(error instanceof ConnectError)) {
    return fallback;
  }
  switch (error.code) {
    case Code.Unauthenticated:
      return t("errors.unauthenticated");
    case Code.PermissionDenied:
      return t("errors.permissionDenied");
    case Code.NotFound:
      return t("errors.notFound");
    case Code.AlreadyExists:
      return error.rawMessage || t("errors.alreadyExists");
    case Code.Unavailable:
      return t("errors.unavailable");
    case Code.ResourceExhausted:
      return error.rawMessage || t("errors.resourceExhausted");
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return error.rawMessage || t("errors.invalidArgument");
    default:
      return fallback;
  }
}

/** The display name of a session. */
export function sessionTitle(session: { label: string }, t: Translate): string {
  return session.label === "" ? t("session.untitled") : session.label;
}
