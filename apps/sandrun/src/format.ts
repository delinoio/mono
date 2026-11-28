// SPDX-License-Identifier: Apache-2.0
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { AgentCredential } from "@delino/sandrun-protos/v1/agent_integration_pb";
import {
  AuditAction,
  AuditTargetKind,
} from "@delino/sandrun-protos/v1/audit_pb";
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
} from "@delino/sandrun-protos/v1/common_pb";
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
  invitationState: {
    [InvitationState.UNSPECIFIED]: UNKNOWN,
    [InvitationState.PENDING]: "enum.invitationState.pending",
    [InvitationState.ACCEPTED]: "enum.invitationState.accepted",
    [InvitationState.REVOKED]: "enum.invitationState.revoked",
    [InvitationState.EXPIRED]: "enum.invitationState.expired",
  },
  workloadKind: {
    [WorkloadKind.UNSPECIFIED]: UNKNOWN,
    [WorkloadKind.GITHUB_JOB]: "enum.workloadKind.githubJob",
    [WorkloadKind.AGENT_SESSION]: "enum.workloadKind.agentSession",
    [WorkloadKind.DEV_SESSION]: "enum.workloadKind.devSession",
  },
  priority: {
    [Priority.UNSPECIFIED]: UNKNOWN,
    [Priority.HIGH]: "enum.priority.high",
    [Priority.NORMAL]: "enum.priority.normal",
    [Priority.LOW]: "enum.priority.low",
  },
  networkMode: {
    [NetworkMode.UNSPECIFIED]: UNKNOWN,
    [NetworkMode.OUTBOUND]: "enum.networkMode.outbound",
  },
  requestState: {
    [RequestState.UNSPECIFIED]: UNKNOWN,
    [RequestState.QUEUED]: "enum.requestState.queued",
    [RequestState.STARTING]: "enum.requestState.starting",
    [RequestState.RUNNING]: "enum.requestState.running",
    [RequestState.ENDED]: "enum.requestState.ended",
    [RequestState.CANCELED]: "enum.requestState.canceled",
    [RequestState.EXPIRED]: "enum.requestState.expired",
    [RequestState.FAILED]: "enum.requestState.failed",
  },
  requestEndReason: {
    [RequestEndReason.UNSPECIFIED]: UNKNOWN,
    [RequestEndReason.COMPLETED]: "enum.requestEndReason.completed",
    [RequestEndReason.CANCELED_BY_SOURCE]:
      "enum.requestEndReason.canceledBySource",
    [RequestEndReason.CANCELED_BY_USER]: "enum.requestEndReason.canceledByUser",
    [RequestEndReason.MAX_QUEUE_WAIT]: "enum.requestEndReason.maxQueueWait",
    [RequestEndReason.NO_ELIGIBLE_DEVICE]:
      "enum.requestEndReason.noEligibleDevice",
    [RequestEndReason.RUNNER_START_FAILED]:
      "enum.requestEndReason.runnerStartFailed",
    [RequestEndReason.RUNNER_LOST]: "enum.requestEndReason.runnerLost",
    [RequestEndReason.PROFILE_DELETED]: "enum.requestEndReason.profileDeleted",
    [RequestEndReason.INTEGRATION_REMOVED]:
      "enum.requestEndReason.integrationRemoved",
    [RequestEndReason.SESSION_DELETED]: "enum.requestEndReason.sessionDeleted",
    [RequestEndReason.SESSION_TTL_EXPIRED]:
      "enum.requestEndReason.sessionTtlExpired",
    [RequestEndReason.MEMBER_REMOVED]: "enum.requestEndReason.memberRemoved",
  },
  runnerState: {
    [RunnerState.UNSPECIFIED]: UNKNOWN,
    [RunnerState.CREATING]: "enum.runnerState.creating",
    [RunnerState.PREPARING]: "enum.runnerState.preparing",
    [RunnerState.WARM]: "enum.runnerState.warm",
    [RunnerState.WAITING_FOR_WORK]: "enum.runnerState.waitingForWork",
    [RunnerState.BUSY]: "enum.runnerState.busy",
    [RunnerState.DELETING]: "enum.runnerState.deleting",
    [RunnerState.DELETED]: "enum.runnerState.deleted",
    [RunnerState.FAILED]: "enum.runnerState.failed",
  },
  runnerEndReason: {
    [RunnerEndReason.UNSPECIFIED]: UNKNOWN,
    [RunnerEndReason.WORK_DONE]: "enum.runnerEndReason.workDone",
    [RunnerEndReason.SURPLUS]: "enum.runnerEndReason.surplus",
    [RunnerEndReason.WARM_RECLAIMED]: "enum.runnerEndReason.warmReclaimed",
    [RunnerEndReason.DELETED_BY_USER]: "enum.runnerEndReason.deletedByUser",
    [RunnerEndReason.START_FAILED]: "enum.runnerEndReason.startFailed",
    [RunnerEndReason.SANDBOX_ENDED]: "enum.runnerEndReason.sandboxEnded",
    [RunnerEndReason.CLEAN_UP]: "enum.runnerEndReason.cleanUp",
  },
  ownSandLinkState: {
    [OwnSandLinkState.UNSPECIFIED]: UNKNOWN,
    [OwnSandLinkState.ACTIVE]: "enum.ownSandLinkState.active",
    [OwnSandLinkState.INVALID]: "enum.ownSandLinkState.invalid",
  },
  gitHubInstallationState: {
    [GitHubInstallationState.UNSPECIFIED]: UNKNOWN,
    [GitHubInstallationState.ACTIVE]: "enum.gitHubInstallationState.active",
    [GitHubInstallationState.SUSPENDED]:
      "enum.gitHubInstallationState.suspended",
    [GitHubInstallationState.DELETED]: "enum.gitHubInstallationState.deleted",
  },
  apiKeyState: {
    [ApiKeyState.UNSPECIFIED]: UNKNOWN,
    [ApiKeyState.ACTIVE]: "enum.apiKeyState.active",
    [ApiKeyState.EXPIRED]: "enum.apiKeyState.expired",
    [ApiKeyState.REVOKED]: "enum.apiKeyState.revoked",
  },
  deviceState: {
    [DeviceState.UNSPECIFIED]: UNKNOWN,
    [DeviceState.ONLINE]: "enum.deviceState.online",
    [DeviceState.OFFLINE]: "enum.deviceState.offline",
  },
  credential: {
    [AgentCredential.UNSPECIFIED]: UNKNOWN,
    [AgentCredential.ENVIRONMENT_KEY]: "enum.credential.environmentKey",
    [AgentCredential.SERVICE_ACCOUNT_API_KEY]:
      "enum.credential.serviceAccountApiKey",
    [AgentCredential.WEBHOOK_SECRET]: "enum.credential.webhookSecret",
    [AgentCredential.API_TOKEN]: "enum.credential.apiToken",
    [AgentCredential.OUTPOST_TOKEN]: "enum.credential.outpostToken",
    [AgentCredential.API_KEY]: "enum.credential.apiKey",
  },
  auditAction: {
    [AuditAction.UNSPECIFIED]: UNKNOWN,
    [AuditAction.ORGANIZATION_CREATED]: "enum.auditAction.organizationCreated",
    [AuditAction.ORGANIZATION_DELETED]: "enum.auditAction.organizationDeleted",
    [AuditAction.OWNERSHIP_TRANSFERRED]:
      "enum.auditAction.ownershipTransferred",
    [AuditAction.INVITATION_CREATED]: "enum.auditAction.invitationCreated",
    [AuditAction.INVITATION_REVOKED]: "enum.auditAction.invitationRevoked",
    [AuditAction.MEMBER_JOINED]: "enum.auditAction.memberJoined",
    [AuditAction.MEMBER_ROLE_CHANGED]: "enum.auditAction.memberRoleChanged",
    [AuditAction.MEMBER_REMOVED]: "enum.auditAction.memberRemoved",
    [AuditAction.OWNSAND_LINK_SET]: "enum.auditAction.ownSandLinkSet",
    [AuditAction.OWNSAND_LINK_REMOVED]: "enum.auditAction.ownSandLinkRemoved",
    [AuditAction.GITHUB_INSTALLATION_LINKED]:
      "enum.auditAction.gitHubInstallationLinked",
    [AuditAction.GITHUB_INSTALLATION_UPDATED]:
      "enum.auditAction.gitHubInstallationUpdated",
    [AuditAction.GITHUB_INSTALLATION_UNLINKED]:
      "enum.auditAction.gitHubInstallationUnlinked",
    [AuditAction.AGENT_INTEGRATION_CREATED]:
      "enum.auditAction.agentIntegrationCreated",
    [AuditAction.AGENT_INTEGRATION_UPDATED]:
      "enum.auditAction.agentIntegrationUpdated",
    [AuditAction.AGENT_INTEGRATION_CREDENTIALS_REPLACED]:
      "enum.auditAction.agentIntegrationCredentialsReplaced",
    [AuditAction.AGENT_INTEGRATION_DELETED]:
      "enum.auditAction.agentIntegrationDeleted",
    [AuditAction.RUNNER_PROFILE_CREATED]:
      "enum.auditAction.runnerProfileCreated",
    [AuditAction.RUNNER_PROFILE_UPDATED]:
      "enum.auditAction.runnerProfileUpdated",
    [AuditAction.RUNNER_PROFILE_DELETED]:
      "enum.auditAction.runnerProfileDeleted",
    [AuditAction.REQUEST_CANCELED]: "enum.auditAction.requestCanceled",
    [AuditAction.RUNNER_DELETED]: "enum.auditAction.runnerDeleted",
    [AuditAction.ORGANIZATION_SETTINGS_CHANGED]:
      "enum.auditAction.organizationSettingsChanged",
    [AuditAction.API_KEY_CREATED]: "enum.auditAction.apiKeyCreated",
    [AuditAction.API_KEY_REVOKED]: "enum.auditAction.apiKeyRevoked",
    [AuditAction.DEV_SESSION_CREATED]: "enum.auditAction.devSessionCreated",
    [AuditAction.DEV_SESSION_DELETED]: "enum.auditAction.devSessionDeleted",
  },
  auditTargetKind: {
    [AuditTargetKind.UNSPECIFIED]: UNKNOWN,
    [AuditTargetKind.ORGANIZATION]: "enum.auditTargetKind.organization",
    [AuditTargetKind.MEMBERSHIP]: "enum.auditTargetKind.membership",
    [AuditTargetKind.INVITATION]: "enum.auditTargetKind.invitation",
    [AuditTargetKind.OWNSAND_LINK]: "enum.auditTargetKind.ownSandLink",
    [AuditTargetKind.GITHUB_INSTALLATION]:
      "enum.auditTargetKind.gitHubInstallation",
    [AuditTargetKind.AGENT_INTEGRATION]:
      "enum.auditTargetKind.agentIntegration",
    [AuditTargetKind.RUNNER_PROFILE]: "enum.auditTargetKind.runnerProfile",
    [AuditTargetKind.REQUEST]: "enum.auditTargetKind.request",
    [AuditTargetKind.RUNNER]: "enum.auditTargetKind.runner",
    [AuditTargetKind.API_KEY]: "enum.auditTargetKind.apiKey",
    [AuditTargetKind.DEV_SESSION]: "enum.auditTargetKind.devSession",
  },
} as const satisfies {
  role: Record<Role, MessageKey>;
  invitationState: Record<InvitationState, MessageKey>;
  workloadKind: Record<WorkloadKind, MessageKey>;
  priority: Record<Priority, MessageKey>;
  networkMode: Record<NetworkMode, MessageKey>;
  requestState: Record<RequestState, MessageKey>;
  requestEndReason: Record<RequestEndReason, MessageKey>;
  runnerState: Record<RunnerState, MessageKey>;
  runnerEndReason: Record<RunnerEndReason, MessageKey>;
  ownSandLinkState: Record<OwnSandLinkState, MessageKey>;
  gitHubInstallationState: Record<GitHubInstallationState, MessageKey>;
  apiKeyState: Record<ApiKeyState, MessageKey>;
  deviceState: Record<DeviceState, MessageKey>;
  credential: Record<AgentCredential, MessageKey>;
  auditAction: Record<AuditAction, MessageKey>;
  auditTargetKind: Record<AuditTargetKind, MessageKey>;
};

/** Vendor names, which stay untranslated in every language. */
const VENDOR_NAMES = {
  [AgentVendor.CLAUDE_CODE]: "Claude Code",
  [AgentVendor.CURSOR]: "Cursor",
  [AgentVendor.OPENAI_CODEX]: "OpenAI Codex",
  [AgentVendor.DEVIN]: "Devin",
} as const satisfies Record<
  Exclude<AgentVendor, AgentVendor.UNSPECIFIED>,
  string
>;

/** Guest OS names, which stay untranslated in every language. */
const GUEST_OS_NAMES = {
  [GuestOs.LINUX]: "Linux",
  [GuestOs.MACOS]: "macOS",
  [GuestOs.WINDOWS]: "Windows",
} as const satisfies Record<Exclude<GuestOs, GuestOs.UNSPECIFIED>, string>;

/** The name of an agent vendor, or "Unknown" in the selected language. */
export function vendorName(vendor: AgentVendor, t: Translate): string {
  return vendor === AgentVendor.UNSPECIFIED
    ? t(UNKNOWN)
    : (VENDOR_NAMES[vendor] ?? t(UNKNOWN));
}

/** The name of a guest OS, or "Unknown" in the selected language. */
export function guestOsName(guestOs: GuestOs, t: Translate): string {
  return guestOs === GuestOs.UNSPECIFIED
    ? t(UNKNOWN)
    : (GUEST_OS_NAMES[guestOs] ?? t(UNKNOWN));
}

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

export function formatTime(ts: Timestamp | undefined, lang: string): string {
  const ms = timestampMs(ts);
  if (ms === undefined) return "—";
  return new Date(ms).toLocaleString(lang, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

/** Formats a length of time in milliseconds, for example "1 h 5 min". */
export function formatSpan(ms: number, { lang, t }: Localizer): string {
  const n = new Intl.NumberFormat(lang).format;
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
  if (totalSeconds < 60)
    return t("duration.seconds", { seconds: n(totalSeconds) });
  const minutes = Math.floor(totalSeconds / 60);
  if (minutes < 60) return t("duration.minutes", { minutes: n(minutes) });
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours < 48) {
    return rest === 0
      ? t("duration.hours", { hours: n(hours) })
      : t("duration.hoursMinutes", { hours: n(hours), minutes: n(rest) });
  }
  const days = Math.floor(hours / 24);
  const restHours = hours % 24;
  return restHours === 0
    ? t("duration.days", { days: n(days) })
    : t("duration.daysHours", { days: n(days), hours: n(restHours) });
}

export function durationMs(duration: Duration | undefined): number | undefined {
  if (duration === undefined) return undefined;
  return Number(duration.seconds) * 1000 + Math.floor(duration.nanos / 1e6);
}

export function formatDuration(
  duration: Duration | undefined,
  localizer: Localizer,
): string {
  const ms = durationMs(duration);
  return ms === undefined ? "—" : formatSpan(ms, localizer);
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

/**
 * The message of a failed request in the selected language. A message that
 * the server returns is shown as received.
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

/** A member's display name, or the Logto user ID when it has none. */
export function memberName(
  members: ReadonlyMap<string, string>,
  userId: string,
): string {
  if (userId === "") return "SandRun";
  const name = members.get(userId);
  return name === undefined || name === "" ? userId : name;
}
