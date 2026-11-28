// SPDX-License-Identifier: Apache-2.0
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { billingReasonOf } from "./api.ts";
import {
  BillingAccountRole,
  BillingMeter,
} from "@delino/ownsand-protos/v1/billing_pb";
import {
  AgentUpdateState,
  AgentVersionPolicyMode,
  ApiKeyState,
  Arch,
  BaseImage,
  DeviceState,
  EnrollmentTokenState,
  GuestOs,
  HostOs,
  InvitationState,
  NetworkMode,
  Role,
  SandboxEndReason,
  SandboxState,
  SnapshotExportState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";
import { ACCOUNTS_URL } from "./config.ts";
import {
  enumText,
  type MessageKey,
  SOURCE_LANG,
  sourceText,
  type Text,
} from "./text.ts";

/**
 * Message keys of the display labels of the enums the console shows. Show one
 * with `enumText` (`src/text.ts`).
 */
export const labels = {
  role: {
    [Role.UNSPECIFIED]: "labels.role.unspecified",
    [Role.OWNER]: "labels.role.owner",
    [Role.ADMIN]: "labels.role.admin",
    [Role.MEMBER]: "labels.role.member",
  } satisfies Record<Role, MessageKey>,
  sandboxState: {
    [SandboxState.UNSPECIFIED]: "labels.sandboxState.unspecified",
    [SandboxState.CREATING]: "labels.sandboxState.creating",
    [SandboxState.RUNNING]: "labels.sandboxState.running",
    [SandboxState.PAUSED]: "labels.sandboxState.paused",
    [SandboxState.DELETING]: "labels.sandboxState.deleting",
    [SandboxState.DELETED]: "labels.sandboxState.deleted",
    [SandboxState.FAILED]: "labels.sandboxState.failed",
    [SandboxState.UNREACHABLE]: "labels.sandboxState.unreachable",
  } satisfies Record<SandboxState, MessageKey>,
  endReason: {
    [SandboxEndReason.UNSPECIFIED]: "labels.endReason.unspecified",
    [SandboxEndReason.DELETED_BY_CALLER]: "labels.endReason.deletedByCaller",
    [SandboxEndReason.IDLE_TTL_EXPIRED]: "labels.endReason.idleTtlExpired",
    [SandboxEndReason.MAX_TTL_EXPIRED]: "labels.endReason.maxTtlExpired",
    [SandboxEndReason.DEVICE_REMOVED]: "labels.endReason.deviceRemoved",
    [SandboxEndReason.ORGANIZATION_DELETED]:
      "labels.endReason.organizationDeleted",
    [SandboxEndReason.CREATE_FAILED]: "labels.endReason.createFailed",
    [SandboxEndReason.LOST_ON_DEVICE]: "labels.endReason.lostOnDevice",
  } satisfies Record<SandboxEndReason, MessageKey>,
  deviceState: {
    [DeviceState.UNSPECIFIED]: "labels.deviceState.unspecified",
    [DeviceState.ONLINE]: "labels.deviceState.online",
    [DeviceState.OFFLINE]: "labels.deviceState.offline",
    [DeviceState.REMOVED]: "labels.deviceState.removed",
  } satisfies Record<DeviceState, MessageKey>,
  hostOs: {
    [HostOs.UNSPECIFIED]: "labels.hostOs.unspecified",
    [HostOs.LINUX]: "labels.hostOs.linux",
    [HostOs.MACOS]: "labels.hostOs.macos",
    [HostOs.WINDOWS]: "labels.hostOs.windows",
  } satisfies Record<HostOs, MessageKey>,
  guestOs: {
    [GuestOs.UNSPECIFIED]: "labels.guestOs.unspecified",
    [GuestOs.LINUX]: "labels.guestOs.linux",
    [GuestOs.MACOS]: "labels.guestOs.macos",
    [GuestOs.WINDOWS]: "labels.guestOs.windows",
  } satisfies Record<GuestOs, MessageKey>,
  arch: {
    [Arch.UNSPECIFIED]: "labels.arch.unspecified",
    [Arch.AMD64]: "labels.arch.amd64",
    [Arch.ARM64]: "labels.arch.arm64",
  } satisfies Record<Arch, MessageKey>,
  baseImage: {
    [BaseImage.UNSPECIFIED]: "labels.baseImage.unspecified",
    [BaseImage.LINUX]: "labels.baseImage.linux",
    [BaseImage.MACOS]: "labels.baseImage.macos",
    [BaseImage.WINDOWS]: "labels.baseImage.windows",
  } satisfies Record<BaseImage, MessageKey>,
  network: {
    [NetworkMode.UNSPECIFIED]: "labels.network.unspecified",
    [NetworkMode.OUTBOUND]: "labels.network.outbound",
    [NetworkMode.NONE]: "labels.network.none",
  } satisfies Record<NetworkMode, MessageKey>,
  updateState: {
    [AgentUpdateState.UNSPECIFIED]: "labels.updateState.unspecified",
    [AgentUpdateState.UP_TO_DATE]: "labels.updateState.upToDate",
    [AgentUpdateState.PENDING_IDLE]: "labels.updateState.pendingIdle",
    [AgentUpdateState.DOWNLOADING]: "labels.updateState.downloading",
    [AgentUpdateState.INSTALLING]: "labels.updateState.installing",
    [AgentUpdateState.FAILED]: "labels.updateState.failed",
    [AgentUpdateState.REVOKED_PENDING_IDLE]:
      "labels.updateState.revokedPendingIdle",
  } satisfies Record<AgentUpdateState, MessageKey>,
  tokenState: {
    [EnrollmentTokenState.UNSPECIFIED]: "labels.tokenState.unspecified",
    [EnrollmentTokenState.PENDING]: "labels.tokenState.pending",
    [EnrollmentTokenState.USED]: "labels.tokenState.used",
    [EnrollmentTokenState.REVOKED]: "labels.tokenState.revoked",
    [EnrollmentTokenState.EXPIRED]: "labels.tokenState.expired",
  } satisfies Record<EnrollmentTokenState, MessageKey>,
  invitationState: {
    [InvitationState.UNSPECIFIED]: "labels.invitationState.unspecified",
    [InvitationState.PENDING]: "labels.invitationState.pending",
    [InvitationState.ACCEPTED]: "labels.invitationState.accepted",
    [InvitationState.REVOKED]: "labels.invitationState.revoked",
    [InvitationState.EXPIRED]: "labels.invitationState.expired",
  } satisfies Record<InvitationState, MessageKey>,
  apiKeyState: {
    [ApiKeyState.UNSPECIFIED]: "labels.apiKeyState.unspecified",
    [ApiKeyState.ACTIVE]: "labels.apiKeyState.active",
    [ApiKeyState.REVOKED]: "labels.apiKeyState.revoked",
  } satisfies Record<ApiKeyState, MessageKey>,
  snapshotState: {
    [SnapshotState.UNSPECIFIED]: "labels.snapshotState.unspecified",
    [SnapshotState.CREATING]: "labels.snapshotState.creating",
    [SnapshotState.READY]: "labels.snapshotState.ready",
    [SnapshotState.FAILED]: "labels.snapshotState.failed",
    [SnapshotState.DELETING]: "labels.snapshotState.deleting",
  } satisfies Record<SnapshotState, MessageKey>,
  exportState: {
    [SnapshotExportState.UNSPECIFIED]: "labels.exportState.unspecified",
    [SnapshotExportState.NOT_EXPORTED]: "labels.exportState.notExported",
    [SnapshotExportState.EXPORTING]: "labels.exportState.exporting",
    [SnapshotExportState.EXPORTED]: "labels.exportState.exported",
    [SnapshotExportState.EXPORT_FAILED]: "labels.exportState.exportFailed",
  } satisfies Record<SnapshotExportState, MessageKey>,
  billingMeter: {
    [BillingMeter.UNSPECIFIED]: "labels.billingMeter.unspecified",
    [BillingMeter.RELAY_EGRESS]: "labels.billingMeter.relayEgress",
    [BillingMeter.R2_STORAGE]: "labels.billingMeter.r2Storage",
    [BillingMeter.CLASS_A_RPC]: "labels.billingMeter.classARpc",
    [BillingMeter.CLASS_B_RPC]: "labels.billingMeter.classBRpc",
  } satisfies Record<BillingMeter, MessageKey>,
  billingAccountRole: {
    [BillingAccountRole.UNSPECIFIED]: "labels.billingAccountRole.unspecified",
    [BillingAccountRole.OWNER]: "labels.billingAccountRole.owner",
    [BillingAccountRole.ADMIN]: "labels.billingAccountRole.admin",
    [BillingAccountRole.VIEWER]: "labels.billingAccountRole.viewer",
  } satisfies Record<BillingAccountRole, MessageKey>,
  policyMode: {
    [AgentVersionPolicyMode.UNSPECIFIED]: "labels.policyMode.unspecified",
    [AgentVersionPolicyMode.FOLLOW_DEFAULT]: "labels.policyMode.followDefault",
    [AgentVersionPolicyMode.PIN]: "labels.policyMode.pin",
    [AgentVersionPolicyMode.HOLD]: "labels.policyMode.hold",
  } satisfies Record<AgentVersionPolicyMode, MessageKey>,
  /**
   * The badge text of an unreachable sandbox with the state it last
   * reported. One whole message per state, so a translation never combines
   * fragments.
   */
  sandboxStateLastKnown: {
    [SandboxState.UNSPECIFIED]: "labels.sandboxStateLastKnown.unspecified",
    [SandboxState.CREATING]: "labels.sandboxStateLastKnown.creating",
    [SandboxState.RUNNING]: "labels.sandboxStateLastKnown.running",
    [SandboxState.PAUSED]: "labels.sandboxStateLastKnown.paused",
    [SandboxState.DELETING]: "labels.sandboxStateLastKnown.deleting",
    [SandboxState.DELETED]: "labels.sandboxStateLastKnown.deleted",
    [SandboxState.FAILED]: "labels.sandboxStateLastKnown.failed",
    [SandboxState.UNREACHABLE]: "labels.sandboxStateLastKnown.unreachable",
  } satisfies Record<SandboxState, MessageKey>,
} as const;

/**
 * Formats a number for `lang`. `fractionDigits` fixes the number of fraction
 * digits; `grouping` false keeps the digits without group separators.
 */
function formatNumber(
  value: number | bigint | Intl.StringNumericLiteral,
  lang: string,
  options: { fractionDigits?: number; grouping?: boolean } = {},
): string {
  return new Intl.NumberFormat(lang, {
    minimumFractionDigits: options.fractionDigits,
    maximumFractionDigits: options.fractionDigits,
    useGrouping: options.grouping ?? true,
  }).format(value);
}

/** Valid vCPU overcommit percentages: 100 to 400 in steps of 25. */
export const OVERCOMMIT_PERCENTAGES: readonly number[] = Array.from(
  { length: 13 },
  (_, i) => 100 + i * 25,
);

/** A non-negative integer from a possibly fractional or invalid number. */
function wholeCount(n: number): bigint {
  return Number.isFinite(n) && n > 0 ? BigInt(Math.floor(n)) : 0n;
}

/**
 * Effective vCPU limit: floor(vCPU limit × percent / 100), in integers.
 * The device page calls it with the live value of a number input, which can
 * be fractional (for example, "1.5") while the user edits it. Floor the
 * inputs to non-negative integers before calculating the effective limit.
 */
export function effectiveVcpuLimit(
  vcpuCount: number,
  overcommitPercent: number,
): number {
  return Number((wholeCount(vcpuCount) * wholeCount(overcommitPercent)) / 100n);
}

/**
 * Formats a whole count, such as vCPUs or GiB of disk, for `lang` without
 * group separators, like the inputs in which the user types it.
 */
export function formatPlainNumber(
  value: number | bigint,
  lang: string = SOURCE_LANG,
): string {
  return formatNumber(value, lang, { grouping: false });
}

/** Formats a whole percentage, for example 125 as `125%`, for `lang`. */
export function formatPercent(
  percent: number,
  lang: string = SOURCE_LANG,
): string {
  return new Intl.NumberFormat(lang, { style: "percent" }).format(
    percent / 100,
  );
}

/** The int32 maximum, which the int32 `Device.effective_vcpu_limit` caps at. */
export const MAX_INT32 = 2147483647;

/**
 * The exact allocated vCPUs of a device. `allocation.vcpuTotalCount` is the
 * exact 64-bit total; a server that predates it leaves it unset, and the
 * int32 `allocation.vcpuCount` (capped at 2147483647 for a larger total) is
 * then the best value. The result is a bigint because the field is an int64.
 */
export function deviceAllocatedVcpu(device: {
  allocation?: { vcpuCount: number; vcpuTotalCount?: bigint | undefined };
}): bigint {
  const a = device.allocation;
  if (a === undefined) return 0n;
  return a.vcpuTotalCount ?? BigInt(a.vcpuCount);
}

/**
 * Formats a memory size in MiB for `lang`, without group separators. The
 * IEC unit symbols are the same in every language.
 */
export function formatMib(
  mib: bigint | number,
  lang: string = SOURCE_LANG,
): string {
  const n = Number(mib);
  const plain = { grouping: false };
  if (n >= 1024 && n % 1024 === 0)
    return `${formatNumber(n / 1024, lang, plain)} GiB`;
  if (n >= 1024)
    return `${formatNumber(n / 1024, lang, { ...plain, fractionDigits: 1 })} GiB`;
  return `${formatNumber(n, lang, plain)} MiB`;
}

/**
 * Formats a byte count with one decimal in the largest binary unit that
 * keeps the shown number below 1024. A value that rounds to 1024.0 at one
 * decimal (for example, 1,048,575 bytes) moves to the next unit, so the
 * text never starts with "1024.0" below 1 TiB.
 */
export function formatBytes(
  bytes: bigint | number,
  lang: string = SOURCE_LANG,
): string {
  const n = Number(bytes);
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = n;
  let unit = 0;
  // Bytes show as an integer, so they promote at exactly 1024. The other
  // units show one decimal, so they promote when that decimal text reaches
  // 1024.0.
  const reachesNextUnit = (v: number, u: number): boolean =>
    u === 0 ? v >= 1024 : Number(v.toFixed(1)) >= 1024;
  while (unit < units.length - 1 && reachesNextUnit(value, unit)) {
    value /= 1024;
    unit++;
  }
  const shown = formatNumber(value, lang, {
    grouping: false,
    fractionDigits: unit === 0 ? undefined : 1,
  });
  return `${shown} ${units[unit]}`;
}

/**
 * Formats a protobuf Duration as days, hours and minutes. A positive
 * duration below one second (`seconds` 0 and `nanos` > 0) shows in whole
 * milliseconds, for example `500 ms`, or `<1 ms` below one millisecond, so
 * it never reads as `0 s`. Other values keep their whole-second text.
 */
export function formatDuration(
  duration: Duration | undefined,
  text: Text = sourceText,
): string {
  if (duration === undefined) return "—";
  const total = Number(duration.seconds);
  const days = Math.floor(total / 86_400);
  const hours = Math.floor((total % 86_400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const value = (n: number) => formatNumber(n, text.lang);
  const parts: string[] = [];
  if (days > 0)
    parts.push(text.t("format.duration.days", { value: value(days) }));
  if (hours > 0)
    parts.push(text.t("format.duration.hours", { value: value(hours) }));
  if (minutes > 0)
    parts.push(text.t("format.duration.minutes", { value: value(minutes) }));
  if (parts.length === 0) {
    if (total === 0 && duration.nanos > 0) {
      const millis = Math.floor(duration.nanos / 1e6);
      return millis > 0
        ? text.t("format.duration.milliseconds", { value: value(millis) })
        : text.t("format.duration.belowOneMillisecond");
    }
    parts.push(text.t("format.duration.seconds", { value: value(total % 60) }));
  }
  return parts.join(" ");
}

/** Formats a protobuf Timestamp for `lang`. */
export function formatTime(
  ts: Timestamp | undefined,
  lang: string = SOURCE_LANG,
): string {
  if (ts === undefined) return "—";
  const ms = Number(ts.seconds) * 1000 + Math.floor(ts.nanos / 1e6);
  return new Date(ms).toLocaleString(lang, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

/**
 * The text of a Device option in the Create sandbox dialog: the device name
 * and its state. It is one string, because the select derives the text of its
 * trigger from the option children.
 */
export function deviceOptionLabel(
  name: string,
  state: DeviceState,
  text: Text = sourceText,
): string {
  return text.t("format.deviceOption", {
    name,
    state: enumText(text, labels.deviceState, state),
  });
}

/**
 * A short, plain message for an RPC error. It never includes request data
 * or credentials, only the server's message for validation errors, which is
 * shown as received.
 */
export function errorMessage(
  error: unknown,
  text: Pick<Text, "t"> = sourceText,
): string {
  if (!(error instanceof ConnectError)) {
    return text.t("errors.generic");
  }
  switch (error.code) {
    case Code.Unauthenticated:
      return text.t("errors.unauthenticated");
    case Code.PermissionDenied:
      return text.t("errors.permissionDenied");
    case Code.NotFound:
      return text.t("errors.notFound");
    case Code.AlreadyExists:
      return error.rawMessage || text.t("errors.alreadyExists");
    case Code.Unavailable:
      return text.t("errors.unavailable");
    case Code.ResourceExhausted:
      // A billing rejection is terminal: show the server's message, which
      // names the reason and links to the billing site.
      if (billingReasonOf(error) !== undefined) {
        return (
          error.rawMessage || text.t("errors.billing", { url: ACCOUNTS_URL })
        );
      }
      return text.t("errors.resourceExhausted");
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return error.rawMessage || text.t("errors.invalidRequest");
    default:
      return text.t("errors.generic");
  }
}

/**
 * Divides `n` by the positive `d` and rounds half away from zero, in exact
 * bigint arithmetic. Billing display rounding uses this mode only.
 */
function divideRoundHalfAway(n: bigint, d: bigint): bigint {
  const quotient = n / d;
  const remainder = n % d;
  const twice = 2n * (remainder < 0n ? -remainder : remainder);
  if (twice < d) return quotient;
  return n < 0n ? quotient - 1n : quotient + 1n;
}

/**
 * The exact decimal text of a scaled value, for example `-1234.57`, which
 * `Intl.NumberFormat` formats without converting it to a binary float.
 */
function decimalLiteral(value: {
  negative: boolean;
  whole: string;
  fraction: string;
}): Intl.StringNumericLiteral {
  const sign = value.negative ? "-" : "";
  return (
    value.fraction === ""
      ? `${sign}${value.whole}`
      : `${sign}${value.whole}.${value.fraction}`
  ) as Intl.StringNumericLiteral;
}

/** Formats a USD amount given as an exact decimal text for `lang`. */
function formatUsd(
  amount: Intl.StringNumericLiteral,
  lang: string,
  maximumFractionDigits: number,
): string {
  return new Intl.NumberFormat(lang, {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits,
  }).format(amount);
}

/**
 * The decimal digits of n / d with exactly `decimals` fraction digits,
 * rounded half away from zero, without group separators. A value that rounds
 * to zero has no sign.
 */
function scaledDecimal(
  n: bigint,
  d: bigint,
  decimals: number,
): { negative: boolean; whole: string; fraction: string } {
  const scale = 10n ** BigInt(decimals);
  const scaled = divideRoundHalfAway(n * scale, d);
  const negative = scaled < 0n;
  const magnitude = negative ? -scaled : scaled;
  return {
    negative,
    whole: (magnitude / scale).toString(),
    fraction: (magnitude % scale).toString().padStart(decimals, "0"),
  };
}

const MICROS_PER_USD = 1_000_000n;

/**
 * Usage costs whose magnitude is below this amount ($0.01) show with up to
 * 6 decimals, because 2 decimals would hide them.
 */
export const SMALL_COST_USD_MICROS = 10_000n;

/**
 * Formats a micro-USD amount (an int64 `_usd_micros` value) as USD with 2
 * decimals for `lang`, for example `$1,234.57`, or `-$0.01` for debt. The
 * amount stays a bigint; display rounding is half away from zero.
 */
export function formatUsdMicros(
  amountUsdMicros: bigint,
  lang: string = SOURCE_LANG,
): string {
  return formatUsd(
    decimalLiteral(scaledDecimal(amountUsdMicros, MICROS_PER_USD, 2)),
    lang,
    2,
  );
}

/**
 * Formats the cost of a usage meter. Zero and a cost of at least $0.01 in
 * magnitude show like `formatUsdMicros`. A smaller nonzero cost shows
 * exactly, with up to 6 decimals and without trailing zeros, for example
 * `$0.005` or `$0.000001`; it needs no rounding, and at least 3 decimals
 * remain because the first two are zero.
 */
export function formatUsageCostUsdMicros(
  amountUsdMicros: bigint,
  lang: string = SOURCE_LANG,
): string {
  const negative = amountUsdMicros < 0n;
  const magnitude = negative ? -amountUsdMicros : amountUsdMicros;
  if (magnitude === 0n || magnitude >= SMALL_COST_USD_MICROS) {
    return formatUsdMicros(amountUsdMicros, lang);
  }
  const fraction = magnitude.toString().padStart(6, "0").replace(/0+$/, "");
  return formatUsd(decimalLiteral({ negative, whole: "0", fraction }), lang, 6);
}

/**
 * Byte-seconds in one GiB-month: 2^30 bytes for 730 hours (2,628,000
 * seconds), the month of the R2 storage price (1 GiB-month = 730
 * GiB-hours).
 */
export const BYTE_SECONDS_PER_GIB_MONTH = 2n ** 30n * 3_600n * 730n;

/**
 * Formats an R2 storage quantity in byte-seconds as GiB-months with 3
 * decimals, rounded half away from zero. A positive quantity that rounds to
 * zero shows as "less than 0.001 GiB-months".
 */
export function formatGibMonths(
  byteSeconds: bigint,
  text: Text = sourceText,
): string {
  const scaled = scaledDecimal(byteSeconds, BYTE_SECONDS_PER_GIB_MONTH, 3);
  const threeDecimals = { fractionDigits: 3 };
  if (byteSeconds > 0n && scaled.whole === "0" && scaled.fraction === "000") {
    return text.t("format.gibMonthsBelowMinimum", {
      value: formatNumber(
        "0.001" as Intl.StringNumericLiteral,
        text.lang,
        threeDecimals,
      ),
    });
  }
  return text.t("format.gibMonths", {
    value: formatNumber(decimalLiteral(scaled), text.lang, threeDecimals),
  });
}

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"] as const;

/**
 * Formats an exact byte count, such as relay egress, in the largest binary
 * unit in which it shows as at least 1.00, with 2 decimals rounded half away
 * from zero. Fewer than 1,024 bytes show as an integer.
 */
export function formatByteCount(
  bytes: bigint,
  lang: string = SOURCE_LANG,
): string {
  const negative = bytes < 0n;
  const magnitude = negative ? -bytes : bytes;
  for (let unit = BYTE_UNITS.length - 1; unit > 0; unit--) {
    const divisor = 1024n ** BigInt(unit);
    if (divideRoundHalfAway(magnitude * 100n, divisor) >= 100n) {
      const { whole, fraction } = scaledDecimal(magnitude, divisor, 2);
      const value = decimalLiteral({ negative, whole, fraction });
      return `${formatNumber(value, lang, { fractionDigits: 2 })} ${BYTE_UNITS[unit]}`;
    }
  }
  return `${formatNumber(bytes, lang)} B`;
}

/** Formats an exact count, such as RPC calls, with group separators for `lang`. */
export function formatCount(count: bigint, lang: string = SOURCE_LANG): string {
  return formatNumber(count, lang);
}
