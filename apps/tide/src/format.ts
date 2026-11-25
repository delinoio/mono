// SPDX-License-Identifier: Apache-2.0
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { AgentCredential } from "@delino/tide-protos/v1/agent_integration_pb";
import { AuditAction, AuditTargetKind } from "@delino/tide-protos/v1/audit_pb";
import {
  AgentVendor,
  ApiKeyState,
  DeviceState,
  GitHubInstallationState,
  GuestOs,
  InvitationState,
  NetworkMode,
  OwnSandLinkState,
  Priority,
  RequestEndReason,
  RequestState,
  Role,
  RunnerEndReason,
  RunnerState,
  WorkloadKind,
} from "@delino/tide-protos/v1/common_pb";

/** Turns an enum value name such as `CANCELED_BY_USER` into "Canceled by user". */
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
  invitationState: enumLabels(InvitationState),
  workloadKind: enumLabels(WorkloadKind, {
    GITHUB_JOB: "GitHub job",
    AGENT_SESSION: "Agent session",
    DEV_SESSION: "Dev session",
  }),
  vendor: enumLabels(AgentVendor, {
    CLAUDE_CODE: "Claude Code",
    CURSOR: "Cursor",
    OPENAI_CODEX: "OpenAI Codex",
    DEVIN: "Devin",
  }),
  guestOs: enumLabels(GuestOs, {
    LINUX: "Linux",
    MACOS: "macOS",
    WINDOWS: "Windows",
  }),
  priority: enumLabels(Priority),
  networkMode: enumLabels(NetworkMode, { OUTBOUND: "Outbound only" }),
  requestState: enumLabels(RequestState),
  requestEndReason: enumLabels(RequestEndReason, {
    MAX_QUEUE_WAIT: "Maximum queue wait reached",
    SESSION_TTL_EXPIRED: "Session TTL ended",
  }),
  runnerState: enumLabels(RunnerState),
  runnerEndReason: enumLabels(RunnerEndReason),
  ownSandLinkState: enumLabels(OwnSandLinkState),
  gitHubInstallationState: enumLabels(GitHubInstallationState),
  apiKeyState: enumLabels(ApiKeyState),
  deviceState: enumLabels(DeviceState),
  credential: enumLabels(AgentCredential, {
    ENVIRONMENT_KEY: "Environment key",
    SERVICE_ACCOUNT_API_KEY: "Service account API key",
    WEBHOOK_SECRET: "Webhook secret",
    API_KEY: "OpenAI API key",
    API_TOKEN: "API token",
    OUTPOST_TOKEN: "Outpost token",
  }),
  auditAction: enumLabels(AuditAction, {
    OWNSAND_LINK_SET: "OwnSand link set",
    OWNSAND_LINK_REMOVED: "OwnSand link removed",
    GITHUB_INSTALLATION_LINKED: "GitHub installation linked",
    GITHUB_INSTALLATION_UPDATED: "GitHub installation updated",
    GITHUB_INSTALLATION_UNLINKED: "GitHub installation unlinked",
    API_KEY_CREATED: "API key created",
    API_KEY_REVOKED: "API key revoked",
  }),
  auditTargetKind: enumLabels(AuditTargetKind, {
    OWNSAND_LINK: "OwnSand link",
    GITHUB_INSTALLATION: "GitHub installation",
    API_KEY: "API key",
  }),
};

/** Terminal request states. */
export const TERMINAL_REQUEST_STATES: ReadonlySet<RequestState> = new Set([
  RequestState.ENDED,
  RequestState.CANCELED,
  RequestState.EXPIRED,
  RequestState.FAILED,
]);

/** Terminal runner states. */
export const TERMINAL_RUNNER_STATES: ReadonlySet<RunnerState> = new Set([
  RunnerState.DELETED,
  RunnerState.FAILED,
]);

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

export function durationMs(duration: Duration | undefined): number | undefined {
  if (duration === undefined) return undefined;
  return Number(duration.seconds) * 1000 + Math.floor(duration.nanos / 1e6);
}

export function formatDuration(duration: Duration | undefined): string {
  const ms = durationMs(duration);
  return ms === undefined ? "—" : formatSpan(ms);
}

/**
 * The queue wait of a request: from `queued_at` to `started_at`, to
 * `ended_at` for a request that never started, or to `now` while it waits.
 */
export function queueWaitMs(
  request: {
    queuedAt?: Timestamp;
    startedAt?: Timestamp;
    endedAt?: Timestamp;
  },
  now: number,
): number | undefined {
  const queued = timestampMs(request.queuedAt);
  if (queued === undefined) return undefined;
  const until =
    timestampMs(request.startedAt) ?? timestampMs(request.endedAt) ?? now;
  return Math.max(0, until - queued);
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
      return "tide is not available right now. Try again in a moment.";
    case Code.ResourceExhausted:
      return error.rawMessage || "A limit was reached. Try again later.";
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return error.rawMessage || "The request is not valid.";
    default:
      return fallback;
  }
}

/** A member's display name, or the Logto user ID when it has none. */
export function memberName(
  members: ReadonlyMap<string, string>,
  userId: string,
): string {
  if (userId === "") return "tide";
  const name = members.get(userId);
  return name === undefined || name === "" ? userId : name;
}
