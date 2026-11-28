// SPDX-License-Identifier: Apache-2.0
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  AgentActivity,
  AgentVendor,
  BridgeErrorKind,
  ConfigOptionCategory,
  CredentialScope,
  CredentialUpdateSource,
  GuestOs,
  InputRejectionReason,
  PermissionMode,
  PermissionOptionKind,
  PlanEntryPriority,
  PlanEntryStatus,
  ProfileIneligibility,
  Role,
  SessionEndReason,
  SessionState,
  StopReason,
  ToolCallStatus,
  ToolKind,
  VendorCredentialKind,
} from "@delino/sandcrew-protos/v1/common_pb";

/** Turns an enum value name such as `WAITING_FOR_INPUT` into "Waiting for input". */
function humanize(name: string): string {
  const text = name.toLowerCase().replaceAll("_", " ");
  return text.charAt(0).toUpperCase() + text.slice(1);
}

/** Labels of every value of a protobuf-es enum, by value. */
function enumLabels<E extends Record<string, string | number>>(
  values: E,
  overrides: Partial<Record<keyof E, string>> = {},
): Record<number, string> {
  const out: Record<number, string> = {};
  for (const [name, value] of Object.entries(values)) {
    if (typeof value !== "number") continue;
    out[value] =
      (overrides as Record<string, string | undefined>)[name] ??
      (name === "UNSPECIFIED" ? "Unknown" : humanize(name));
  }
  return out;
}

/** Display labels of the enums the console shows. */
export const labels = {
  role: enumLabels(Role),
  vendor: enumLabels(AgentVendor, {
    CLAUDE_CODE: "Claude Code",
    OPENAI_CODEX: "OpenAI Codex",
  }),
  guestOs: enumLabels(GuestOs, {
    LINUX: "Linux",
    MACOS: "macOS",
    WINDOWS: "Windows",
  }),
  sessionState: enumLabels(SessionState),
  endReason: enumLabels(SessionEndReason, {
    START_FAILED: "The session could not start",
    QUEUE_EXPIRED: "The session waited too long in the queue",
    AGENT_EXITED: "The agent exited",
    DEVICE_LOST: "The device of the session was lost",
    RESTORE_FAILED: "The saved state could not be restored",
    PAUSED_RETENTION_EXPIRED: "The paused-session retention ended",
    BRIDGE_UNSUPPORTED: "The session uses an unsupported bridge version",
    EXECUTOR_ENDED: "The SandRun dev session ended",
  }),
  permissionMode: enumLabels(PermissionMode, {
    AUTO_APPROVE: "Auto-approve",
    ASK: "Ask before tools run",
  }),
  credentialScope: enumLabels(CredentialScope),
  credentialKind: enumLabels(VendorCredentialKind, {
    API_KEY: "API key",
    CLAUDE_OAUTH_TOKEN: "Claude Code token",
    CODEX_AUTH_JSON: "Codex auth.json",
  }),
  credentialUpdateSource: enumLabels(CredentialUpdateSource, {
    USER: "you or an administrator",
    AGENT_REFRESH: "an agent refresh",
  }),
  ineligibility: enumLabels(ProfileIneligibility, {
    HAS_MAXIMUM_LIFETIME: "The profile has a maximum lifetime",
    NETWORK_NOT_OUTBOUND: "The network mode is not outbound only",
    GUEST_OS_UNSUPPORTED: "SandCrew does not support this guest OS yet",
  }),
  toolKind: enumLabels(ToolKind),
  toolCallStatus: enumLabels(ToolCallStatus),
  planPriority: enumLabels(PlanEntryPriority),
  planStatus: enumLabels(PlanEntryStatus),
  permissionOptionKind: enumLabels(PermissionOptionKind),
  stopReason: enumLabels(StopReason, {
    END_TURN: "Turn ended",
    MAX_TOKENS: "Turn ended: token limit reached",
    MAX_TURN_REQUESTS: "Turn ended: request limit reached",
    REFUSAL: "Turn ended: the agent refused",
    CANCELLED: "Turn stopped",
  }),
  configCategory: enumLabels(ConfigOptionCategory),
  agentActivity: enumLabels(AgentActivity),
  bridgeError: enumLabels(BridgeErrorKind, {
    AGENT_INSTALL_FAILED: "The agent could not be installed",
    CHECKSUM_MISMATCH: "The agent download did not match its checksum",
    AGENT_START_FAILED: "The agent could not start",
    CREDENTIAL_MISSING: "The vendor credential is missing",
    ACP_PROTOCOL_ERROR: "The agent sent an invalid message",
    INPUT_INVALID: "An input was not valid",
    OPTION_REJECTED: "The agent rejected an option",
    EVENT_LOG_WRITE_FAILED: "The event log could not be written",
    CREDENTIAL_WATCH_FAILED: "Credential changes could not be watched",
  }),
  inputRejection: enumLabels(InputRejectionReason),
};

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

export function formatTime(ts: Timestamp | undefined): string {
  const ms = timestampMs(ts);
  if (ms === undefined) return "—";
  return new Date(ms).toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

export function durationMs(duration: Duration | undefined): number | undefined {
  if (duration === undefined) return undefined;
  return Number(duration.seconds) * 1000 + Math.floor(duration.nanos / 1e6);
}

/** Formats a length of time in milliseconds, for example "1 h 5 min". */
export function formatSpan(ms: number): string {
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
  if (totalSeconds < 60) return `${totalSeconds} s`;
  const minutes = Math.floor(totalSeconds / 60);
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours < 48) return rest === 0 ? `${hours} h` : `${hours} h ${rest} min`;
  const days = Math.floor(hours / 24);
  const restHours = hours % 24;
  return restHours === 0 ? `${days} d` : `${days} d ${restHours} h`;
}

export function errorMessage(
  error: unknown,
  fallback = "Something went wrong. Try again.",
): string {
  if (!(error instanceof ConnectError)) {
    return fallback;
  }
  switch (error.code) {
    case Code.Unauthenticated:
      return "Your session ended. Sign in again.";
    case Code.PermissionDenied:
      return "You do not have permission to do this.";
    case Code.NotFound:
      return "It was not found, or you no longer have access to it.";
    case Code.AlreadyExists:
      return error.rawMessage || "It already exists.";
    case Code.Unavailable:
      return "SandCrew is not available right now. Try again in a moment.";
    case Code.ResourceExhausted:
      return error.rawMessage || "A limit was reached. Try again later.";
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return error.rawMessage || "The request is not valid.";
    default:
      return fallback;
  }
}

/** The display name of a session. */
export function sessionTitle(session: { label: string }): string {
  return session.label === "" ? "Untitled session" : session.label;
}
