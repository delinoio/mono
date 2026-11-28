// SPDX-License-Identifier: Apache-2.0
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  BillingAccountRole,
  InvitationState,
} from "@delino/accounts-protos/v1/billing_account_pb";
import {
  LedgerEntryKind,
  SubscriptionStatus,
} from "@delino/accounts-protos/v1/credit_pb";
import {
  BillingStatus,
  BillingStatusReason,
  Meter,
  ServiceId,
} from "@delino/accounts-protos/v1/usage_pb";
import {
  englishText,
  type MessageKey,
  type PluralKey,
  type Text,
} from "./text.ts";

/**
 * Catalog keys of the enum values the pages show. Pass a key to `t` of
 * `useI18n()` to show the text.
 */
export const labels = {
  role: {
    [BillingAccountRole.UNSPECIFIED]: "role.unknown",
    [BillingAccountRole.OWNER]: "role.owner",
    [BillingAccountRole.ADMIN]: "role.admin",
    [BillingAccountRole.VIEWER]: "role.viewer",
  } as const satisfies Record<BillingAccountRole, MessageKey>,
  invitationState: {
    [InvitationState.UNSPECIFIED]: "invitationState.unknown",
    [InvitationState.PENDING]: "invitationState.pending",
    [InvitationState.ACCEPTED]: "invitationState.accepted",
    [InvitationState.REVOKED]: "invitationState.revoked",
    [InvitationState.EXPIRED]: "invitationState.expired",
  } as const satisfies Record<InvitationState, MessageKey>,
  subscriptionStatus: {
    [SubscriptionStatus.UNSPECIFIED]: "subscriptionStatus.unknown",
    [SubscriptionStatus.NONE]: "subscriptionStatus.none",
    [SubscriptionStatus.PENDING]: "subscriptionStatus.pending",
    [SubscriptionStatus.ACTIVE]: "subscriptionStatus.active",
    [SubscriptionStatus.PAST_DUE]: "subscriptionStatus.pastDue",
    [SubscriptionStatus.CANCELED]: "subscriptionStatus.canceled",
  } as const satisfies Record<SubscriptionStatus, MessageKey>,
  ledgerKind: {
    [LedgerEntryKind.UNSPECIFIED]: "ledgerKind.unknown",
    [LedgerEntryKind.SUBSCRIPTION_CREDIT]: "ledgerKind.subscriptionCredit",
    [LedgerEntryKind.MANUAL_TOP_UP]: "ledgerKind.manualTopUp",
    [LedgerEntryKind.AUTO_TOP_UP]: "ledgerKind.autoTopUp",
    [LedgerEntryKind.USAGE]: "ledgerKind.usage",
    [LedgerEntryKind.EXPIRY]: "ledgerKind.expiry",
    [LedgerEntryKind.REFUND]: "ledgerKind.refund",
  } as const satisfies Record<LedgerEntryKind, MessageKey>,
  meter: {
    [Meter.UNSPECIFIED]: "meter.unknown",
    [Meter.OWNSAND_RELAY_EGRESS]: "meter.relayEgress",
    [Meter.OWNSAND_R2_STORAGE]: "meter.snapshotStorage",
    [Meter.OWNSAND_CLASS_A_RPC]: "meter.classARpc",
    [Meter.OWNSAND_CLASS_B_RPC]: "meter.classBRpc",
  } as const satisfies Record<Meter, MessageKey>,
  billingStatus: {
    [BillingStatus.UNSPECIFIED]: "billingStatus.unknown",
    [BillingStatus.ALLOWED]: "billingStatus.active",
    [BillingStatus.BLOCKED]: "billingStatus.blocked",
    [BillingStatus.SUSPENDED]: "billingStatus.suspended",
  } as const satisfies Record<BillingStatus, MessageKey>,
  /** A reason without text has no key. */
  billingStatusReason: {
    [BillingStatusReason.UNSPECIFIED]: undefined,
    [BillingStatusReason.NONE]: undefined,
    [BillingStatusReason.NO_ACTIVE_SUBSCRIPTION]:
      "billingStatusReason.noActiveSubscription",
    [BillingStatusReason.NON_POSITIVE_BALANCE]:
      "billingStatusReason.noCreditLeft",
  } as const satisfies Record<BillingStatusReason, MessageKey | undefined>,
};

/**
 * Names of the services. They are product names, so they are not
 * translated; an unknown service uses the catalog text.
 */
const SERVICE_NAMES = {
  [ServiceId.UNSPECIFIED]: undefined,
  [ServiceId.OWNSAND]: "OwnSand",
} as const satisfies Record<ServiceId, string | undefined>;

/** The name of a service. */
export function serviceName(
  service: ServiceId,
  text: Text = englishText,
): string {
  return SERVICE_NAMES[service] ?? text.t("service.unknown");
}

/** Plural groups of the units of the meters, for quantities. */
const METER_UNITS = {
  [Meter.UNSPECIFIED]: undefined,
  [Meter.OWNSAND_RELAY_EGRESS]: "quantity.bytes",
  [Meter.OWNSAND_R2_STORAGE]: "quantity.byteSeconds",
  [Meter.OWNSAND_CLASS_A_RPC]: "quantity.calls",
  [Meter.OWNSAND_CLASS_B_RPC]: "quantity.calls",
} as const satisfies Record<Meter, PluralKey | undefined>;

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
export function serviceLabel(id: string, text: Text = englishText): string {
  const service = SERVICE_IDS.get(id);
  return service === undefined ? id : serviceName(service, text);
}

/** The label of a stable meter identifier; an unknown one shows as is. */
export function meterLabel(id: string, text: Text = englishText): string {
  const meter = meterFromId(id);
  return meter === undefined ? id : text.t(labels.meter[meter]);
}

/** Formats an integer count for the language, exactly (no `number`). */
export function formatCount(value: bigint, lang = englishText.lang): string {
  return new Intl.NumberFormat(lang).format(value);
}

/** Formats a meter quantity with the unit of the meter. */
export function formatQuantity(
  quantity: bigint,
  meter: Meter | undefined,
  text: Text = englishText,
): string {
  const unit = meter === undefined ? undefined : METER_UNITS[meter];
  const count = formatCount(quantity, text.lang);
  // `Number` only selects the plural form; the shown count is the exact
  // `bigint` text.
  return unit === undefined
    ? count
    : text.plural(unit, Number(quantity), { count });
}

function toDate(ts: Timestamp): Date {
  return new Date(Number(ts.seconds) * 1000 + Math.floor(ts.nanos / 1e6));
}

/** Formats a protobuf Timestamp as a date and time in the language. */
export function formatTime(
  ts: Timestamp | undefined,
  lang = englishText.lang,
): string {
  if (ts === undefined) return "—";
  return toDate(ts).toLocaleString(lang, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

/** Formats a protobuf Timestamp as a date in the language. */
export function formatDate(
  ts: Timestamp | undefined,
  lang = englishText.lang,
): string {
  if (ts === undefined) return "—";
  return toDate(ts).toLocaleDateString(lang, { dateStyle: "medium" });
}

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

/** Formats a month in the language, for example `November 2026`. */
export function formatMonth(value: UtcMonth, lang = englishText.lang): string {
  return new Intl.DateTimeFormat(lang, {
    year: "numeric",
    month: "long",
    timeZone: "UTC",
  }).format(Date.UTC(value.year, value.month - 1, 1));
}

/**
 * The `resource_exhausted` message of `CreateInvitation` when the inviter or
 * the address reached its limit in 24 hours (the contract of
 * `servers/accounts-api`).
 */
export const INVITATION_LIMIT_MESSAGE = "too many invitations; try again later";

/**
 * A short, plain message for an RPC error. It never includes request data
 * or credentials, only the server's message for validation errors. A server
 * message is shown as received, untranslated.
 */
export function errorMessage(error: unknown, text: Text = englishText): string {
  if (!(error instanceof ConnectError)) {
    return text.t("errors.generic");
  }
  switch (error.code) {
    case Code.Unauthenticated:
      return text.t("errors.sessionEnded");
    case Code.PermissionDenied:
      return error.rawMessage || text.t("errors.permissionDenied");
    case Code.NotFound:
      return text.t("errors.notFound");
    case Code.AlreadyExists:
      return error.rawMessage || text.t("errors.alreadyExists");
    case Code.Unavailable:
      return text.t("errors.unavailable");
    case Code.ResourceExhausted:
      // Only the invitation limit has a message that is written for the
      // reader. Other resource_exhausted messages, such as the request size
      // limit, are technical, so they keep the generic text.
      return error.rawMessage === INVITATION_LIMIT_MESSAGE
        ? error.rawMessage
        : text.t("errors.tooManyRequests");
    case Code.InvalidArgument:
    case Code.FailedPrecondition:
      return error.rawMessage || text.t("errors.invalidRequest");
    default:
      return text.t("errors.generic");
  }
}
