// SPDX-License-Identifier: Apache-2.0
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
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
} from "ownsand-sdk/gen/common_pb";

/** Display labels of the enums the console shows. */
export const labels = {
  role: {
    [Role.UNSPECIFIED]: "Unknown",
    [Role.OWNER]: "Owner",
    [Role.ADMIN]: "Admin",
    [Role.MEMBER]: "Member",
  },
  sandboxState: {
    [SandboxState.UNSPECIFIED]: "Unknown",
    [SandboxState.CREATING]: "Creating",
    [SandboxState.RUNNING]: "Running",
    [SandboxState.PAUSED]: "Paused",
    [SandboxState.DELETING]: "Deleting",
    [SandboxState.DELETED]: "Deleted",
    [SandboxState.FAILED]: "Failed",
    [SandboxState.UNREACHABLE]: "Unreachable",
  },
  endReason: {
    [SandboxEndReason.UNSPECIFIED]: "Unknown",
    [SandboxEndReason.DELETED_BY_CALLER]: "Deleted by a caller",
    [SandboxEndReason.IDLE_TTL_EXPIRED]: "Idle TTL ended",
    [SandboxEndReason.MAX_TTL_EXPIRED]: "Maximum TTL ended",
    [SandboxEndReason.DEVICE_REMOVED]: "Device removed",
    [SandboxEndReason.ORGANIZATION_DELETED]: "Organization deleted",
    [SandboxEndReason.CREATE_FAILED]: "Create failed",
    [SandboxEndReason.LOST_ON_DEVICE]: "Lost on the device",
  },
  deviceState: {
    [DeviceState.UNSPECIFIED]: "Unknown",
    [DeviceState.ONLINE]: "Online",
    [DeviceState.OFFLINE]: "Offline",
    [DeviceState.REMOVED]: "Removed",
  },
  hostOs: {
    [HostOs.UNSPECIFIED]: "Unknown",
    [HostOs.LINUX]: "Linux",
    [HostOs.MACOS]: "macOS",
    [HostOs.WINDOWS]: "Windows",
  },
  guestOs: {
    [GuestOs.UNSPECIFIED]: "Unknown",
    [GuestOs.LINUX]: "Linux",
    [GuestOs.MACOS]: "macOS",
    [GuestOs.WINDOWS]: "Windows",
  },
  arch: {
    [Arch.UNSPECIFIED]: "Unknown",
    [Arch.AMD64]: "x86-64",
    [Arch.ARM64]: "ARM64",
  },
  baseImage: {
    [BaseImage.UNSPECIFIED]: "Unknown",
    [BaseImage.LINUX]: "Linux",
    [BaseImage.MACOS]: "macOS",
    [BaseImage.WINDOWS]: "Windows",
  },
  network: {
    [NetworkMode.UNSPECIFIED]: "Outbound",
    [NetworkMode.OUTBOUND]: "Outbound",
    [NetworkMode.NONE]: "No network",
  },
  updateState: {
    [AgentUpdateState.UNSPECIFIED]: "Unknown",
    [AgentUpdateState.UP_TO_DATE]: "Up to date",
    [AgentUpdateState.PENDING_IDLE]: "Waits for idle",
    [AgentUpdateState.DOWNLOADING]: "Downloading",
    [AgentUpdateState.INSTALLING]: "Installing",
    [AgentUpdateState.FAILED]: "Update failed",
    [AgentUpdateState.REVOKED_PENDING_IDLE]: "Revoked version, waits for idle",
  },
  tokenState: {
    [EnrollmentTokenState.UNSPECIFIED]: "Unknown",
    [EnrollmentTokenState.PENDING]: "Not used",
    [EnrollmentTokenState.USED]: "Used",
    [EnrollmentTokenState.REVOKED]: "Revoked",
    [EnrollmentTokenState.EXPIRED]: "Expired",
  },
  invitationState: {
    [InvitationState.UNSPECIFIED]: "Unknown",
    [InvitationState.PENDING]: "Pending",
    [InvitationState.ACCEPTED]: "Accepted",
    [InvitationState.REVOKED]: "Revoked",
    [InvitationState.EXPIRED]: "Expired",
  },
  apiKeyState: {
    [ApiKeyState.UNSPECIFIED]: "Unknown",
    [ApiKeyState.ACTIVE]: "Active",
    [ApiKeyState.REVOKED]: "Revoked",
  },
  snapshotState: {
    [SnapshotState.UNSPECIFIED]: "Unknown",
    [SnapshotState.CREATING]: "Creating",
    [SnapshotState.READY]: "Ready",
    [SnapshotState.FAILED]: "Failed",
    [SnapshotState.DELETING]: "Deleting",
  },
  exportState: {
    [SnapshotExportState.UNSPECIFIED]: "Unknown",
    [SnapshotExportState.NOT_EXPORTED]: "Only on its device",
    [SnapshotExportState.EXPORTING]: "Exporting",
    [SnapshotExportState.EXPORTED]: "Exported",
    [SnapshotExportState.EXPORT_FAILED]: "Export failed",
  },
  policyMode: {
    [AgentVersionPolicyMode.UNSPECIFIED]: "Unknown",
    [AgentVersionPolicyMode.FOLLOW_DEFAULT]: "Follow the default version",
    [AgentVersionPolicyMode.PIN]: "Pin a version",
    [AgentVersionPolicyMode.HOLD]: "Hold updates",
  },
} as const;

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
 * be fractional (for example, "1.5") until the input snaps it on Enter or
 * blur, so the inputs are first floored to non-negative integers.
 */
export function effectiveVcpuLimit(
  vcpuCount: number,
  overcommitPercent: number,
): number {
  return Number((wholeCount(vcpuCount) * wholeCount(overcommitPercent)) / 100n);
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

/** Formats a memory size in MiB. */
export function formatMib(mib: bigint | number): string {
  const n = Number(mib);
  if (n >= 1024 && n % 1024 === 0) return `${n / 1024} GiB`;
  if (n >= 1024) return `${(n / 1024).toFixed(1)} GiB`;
  return `${n} MiB`;
}

/** Formats a byte count. */
export function formatBytes(bytes: bigint | number): string {
  const n = Number(bytes);
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${unit === 0 ? value : value.toFixed(1)} ${units[unit]}`;
}

/** Formats a protobuf Duration as hours and minutes. */
export function formatDuration(duration: Duration | undefined): string {
  if (duration === undefined) return "—";
  const total = Number(duration.seconds);
  const days = Math.floor(total / 86_400);
  const hours = Math.floor((total % 86_400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const parts: string[] = [];
  if (days > 0) parts.push(`${days} d`);
  if (hours > 0) parts.push(`${hours} h`);
  if (minutes > 0) parts.push(`${minutes} min`);
  if (parts.length === 0) parts.push(`${total % 60} s`);
  return parts.join(" ");
}

/** Formats a protobuf Timestamp in the user's locale. */
export function formatTime(ts: Timestamp | undefined): string {
  if (ts === undefined) return "—";
  const ms = Number(ts.seconds) * 1000 + Math.floor(ts.nanos / 1e6);
  return new Date(ms).toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

/**
 * A short, plain message for an RPC error. It never includes request data
 * or credentials, only the server's message for validation errors.
 */
export function errorMessage(error: unknown): string {
  if (!(error instanceof ConnectError)) {
    return "Something went wrong. Try again.";
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
      return "OwnSand is not available right now. Try again in a moment.";
    case Code.ResourceExhausted:
      return "Too many requests. Wait a moment and try again.";
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return error.rawMessage || "The request is not valid.";
    default:
      return "Something went wrong. Try again.";
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

/** Inserts a comma between every group of three digits. */
function groupThousands(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/**
 * The decimal text of n / d with exactly `decimals` fraction digits,
 * rounded half away from zero. A value that rounds to zero has no sign.
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
    whole: groupThousands((magnitude / scale).toString()),
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
 * decimals, for example `$1,234.57`, or `-$0.01` for debt. The amount stays
 * a bigint; display rounding is half away from zero.
 */
export function formatUsdMicros(amountUsdMicros: bigint): string {
  const { negative, whole, fraction } = scaledDecimal(
    amountUsdMicros,
    MICROS_PER_USD,
    2,
  );
  return `${negative ? "-" : ""}$${whole}.${fraction}`;
}

/**
 * Formats the cost of a usage meter. Zero and a cost of at least $0.01 in
 * magnitude show like `formatUsdMicros`. A smaller nonzero cost shows
 * exactly, with up to 6 decimals and without trailing zeros, for example
 * `$0.005` or `$0.000001`; it needs no rounding, and at least 3 decimals
 * remain because the first two are zero.
 */
export function formatUsageCostUsdMicros(amountUsdMicros: bigint): string {
  const negative = amountUsdMicros < 0n;
  const magnitude = negative ? -amountUsdMicros : amountUsdMicros;
  if (magnitude === 0n || magnitude >= SMALL_COST_USD_MICROS) {
    return formatUsdMicros(amountUsdMicros);
  }
  const fraction = magnitude.toString().padStart(6, "0").replace(/0+$/, "");
  return `${negative ? "-" : ""}$0.${fraction}`;
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
export function formatGibMonths(byteSeconds: bigint): string {
  const { negative, whole, fraction } = scaledDecimal(
    byteSeconds,
    BYTE_SECONDS_PER_GIB_MONTH,
    3,
  );
  if (byteSeconds > 0n && whole === "0" && fraction === "000") {
    return "less than 0.001 GiB-months";
  }
  return `${negative ? "-" : ""}${whole}.${fraction} GiB-months`;
}

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"] as const;

/**
 * Formats an exact byte count, such as relay egress, in the largest binary
 * unit in which it shows as at least 1.00, with 2 decimals rounded half away
 * from zero. Fewer than 1,024 bytes show as an integer.
 */
export function formatByteCount(bytes: bigint): string {
  const negative = bytes < 0n;
  const magnitude = negative ? -bytes : bytes;
  const sign = negative ? "-" : "";
  for (let unit = BYTE_UNITS.length - 1; unit > 0; unit--) {
    const divisor = 1024n ** BigInt(unit);
    if (divideRoundHalfAway(magnitude * 100n, divisor) >= 100n) {
      const { whole, fraction } = scaledDecimal(magnitude, divisor, 2);
      return `${sign}${whole}.${fraction} ${BYTE_UNITS[unit]}`;
    }
  }
  return `${sign}${groupThousands(magnitude.toString())} B`;
}

/** Formats an exact count, such as RPC calls, with thousands separators. */
export function formatCount(count: bigint): string {
  const negative = count < 0n;
  const magnitude = negative ? -count : count;
  return `${negative ? "-" : ""}${groupThousands(magnitude.toString())}`;
}
