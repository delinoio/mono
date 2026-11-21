// SPDX-License-Identifier: Apache-2.0
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BillingAccountRole,
  InvitationState,
} from "./gen/delino/accounts/v1/billing_account_pb.ts";
import {
  LedgerEntryKind,
  SubscriptionStatus,
} from "./gen/delino/accounts/v1/credit_pb.ts";
import {
  BillingStatus,
  BillingStatusReason,
  Meter,
  ServiceId,
} from "./gen/delino/accounts/v1/usage_pb.ts";

/** Display labels of the enums the pages show. */
export const labels = {
  role: {
    [BillingAccountRole.UNSPECIFIED]: "Unknown",
    [BillingAccountRole.OWNER]: "Owner",
    [BillingAccountRole.ADMIN]: "Admin",
    [BillingAccountRole.VIEWER]: "Viewer",
  },
  invitationState: {
    [InvitationState.UNSPECIFIED]: "Unknown",
    [InvitationState.PENDING]: "Pending",
    [InvitationState.ACCEPTED]: "Accepted",
    [InvitationState.REVOKED]: "Revoked",
    [InvitationState.EXPIRED]: "Expired",
  },
  subscriptionStatus: {
    [SubscriptionStatus.UNSPECIFIED]: "Unknown",
    [SubscriptionStatus.NONE]: "No subscription",
    [SubscriptionStatus.PENDING]: "Waiting for payment",
    [SubscriptionStatus.ACTIVE]: "Active",
    [SubscriptionStatus.PAST_DUE]: "Payment overdue",
    [SubscriptionStatus.CANCELED]: "Canceled",
  },
  ledgerKind: {
    [LedgerEntryKind.UNSPECIFIED]: "Unknown",
    [LedgerEntryKind.SUBSCRIPTION_CREDIT]: "Subscription credit",
    [LedgerEntryKind.MANUAL_TOP_UP]: "Top-up",
    [LedgerEntryKind.AUTO_TOP_UP]: "Automatic top-up",
    [LedgerEntryKind.USAGE]: "Usage",
    [LedgerEntryKind.EXPIRY]: "Expired credit",
    [LedgerEntryKind.REFUND]: "Refund",
  },
  service: {
    [ServiceId.UNSPECIFIED]: "Unknown service",
    [ServiceId.OWNSAND]: "OwnSand",
  },
  meter: {
    [Meter.UNSPECIFIED]: "Unknown meter",
    [Meter.OWNSAND_RELAY_EGRESS]: "Relay egress",
    [Meter.OWNSAND_R2_STORAGE]: "Snapshot storage",
    [Meter.OWNSAND_CLASS_A_RPC]: "Class A API calls",
    [Meter.OWNSAND_CLASS_B_RPC]: "Class B API calls",
  },
  billingStatus: {
    [BillingStatus.UNSPECIFIED]: "Unknown",
    [BillingStatus.ALLOWED]: "Active",
    [BillingStatus.BLOCKED]: "Blocked",
    [BillingStatus.SUSPENDED]: "Suspended",
  },
  billingStatusReason: {
    [BillingStatusReason.UNSPECIFIED]: "",
    [BillingStatusReason.NONE]: "",
    [BillingStatusReason.NO_ACTIVE_SUBSCRIPTION]: "No active subscription",
    [BillingStatusReason.NON_POSITIVE_BALANCE]: "No credit left",
  },
} as const;

/** Units of the meters, for quantities. */
const METER_UNITS: Record<Meter, string> = {
  [Meter.UNSPECIFIED]: "",
  [Meter.OWNSAND_RELAY_EGRESS]: "bytes",
  [Meter.OWNSAND_R2_STORAGE]: "byte-seconds",
  [Meter.OWNSAND_CLASS_A_RPC]: "calls",
  [Meter.OWNSAND_CLASS_B_RPC]: "calls",
};

/**
 * Stable meter identifiers of `LedgerEntry.meter` (`<service>.<meter>`) and
 * their enum values.
 */
const METER_IDS: ReadonlyMap<string, Meter> = new Map([
  ["ownsand.relay_egress", Meter.OWNSAND_RELAY_EGRESS],
  ["ownsand.r2_storage", Meter.OWNSAND_R2_STORAGE],
  ["ownsand.class_a_rpc", Meter.OWNSAND_CLASS_A_RPC],
  ["ownsand.class_b_rpc", Meter.OWNSAND_CLASS_B_RPC],
]);

/** Stable service identifiers of `LedgerEntry.service`. */
const SERVICE_IDS: ReadonlyMap<string, ServiceId> = new Map([
  ["ownsand", ServiceId.OWNSAND],
]);

/** The meter of a stable meter identifier, or undefined for an unknown one. */
export function meterFromId(id: string): Meter | undefined {
  return METER_IDS.get(id);
}

/** The label of a stable service identifier; an unknown one shows as is. */
export function serviceLabel(id: string): string {
  const service = SERVICE_IDS.get(id);
  return service === undefined ? id : labels.service[service];
}

/** The label of a stable meter identifier; an unknown one shows as is. */
export function meterLabel(id: string): string {
  const meter = meterFromId(id);
  return meter === undefined ? id : labels.meter[meter];
}

function groupThousands(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** Formats an integer count with thousands separators. */
export function formatCount(value: bigint): string {
  return value < 0n
    ? `-${groupThousands((-value).toString())}`
    : groupThousands(value.toString());
}

/** Formats a meter quantity with the unit of the meter. */
export function formatQuantity(
  quantity: bigint,
  meter: Meter | undefined,
): string {
  const unit = meter === undefined ? "" : METER_UNITS[meter];
  return unit === ""
    ? formatCount(quantity)
    : `${formatCount(quantity)} ${unit}`;
}

function toDate(ts: Timestamp): Date {
  return new Date(Number(ts.seconds) * 1000 + Math.floor(ts.nanos / 1e6));
}

/** Formats a protobuf Timestamp as an English date and time. */
export function formatTime(ts: Timestamp | undefined): string {
  if (ts === undefined) return "—";
  return toDate(ts).toLocaleString("en-US", {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

/** Formats a protobuf Timestamp as an English date. */
export function formatDate(ts: Timestamp | undefined): string {
  if (ts === undefined) return "—";
  return toDate(ts).toLocaleDateString("en-US", { dateStyle: "medium" });
}

const MONTHS = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
] as const;

/** A UTC calendar month. */
export interface UtcMonth {
  year: number;
  /** 1 to 12. */
  month: number;
}

/** The UTC calendar month of a time. */
export function utcMonthOf(date: Date): UtcMonth {
  return { year: date.getUTCFullYear(), month: date.getUTCMonth() + 1 };
}

/** Moves a month by `delta` months. */
export function addMonths(value: UtcMonth, delta: number): UtcMonth {
  const index = value.year * 12 + (value.month - 1) + delta;
  return { year: Math.floor(index / 12), month: (index % 12) + 1 };
}

/** Formats a month, for example `November 2026`. */
export function formatMonth(value: UtcMonth): string {
  return `${MONTHS[value.month - 1]} ${value.year}`;
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
      return error.rawMessage || "You do not have permission to do this.";
    case Code.NotFound:
      return "It was not found, or you no longer have access to it.";
    case Code.AlreadyExists:
      return error.rawMessage || "It already exists.";
    case Code.Unavailable:
      return "Delino Accounts is not available right now. Try again in a moment.";
    case Code.ResourceExhausted:
      return "Too many requests. Wait a moment and try again.";
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return error.rawMessage || "The request is not valid.";
    default:
      return "Something went wrong. Try again.";
  }
}
