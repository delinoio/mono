// SPDX-License-Identifier: Apache-2.0

/**
 * Money helpers. Every amount is a `bigint` of micro-USD (1 USD =
 * 1,000,000), as the API sends it. Nothing here converts an amount to
 * `number`: parsing works on the decimal text and formatting on integer
 * division, and rounding happens only for display
 * (docs/repository-billing-contract.md).
 */

/** Micro-USD in one USD. */
export const MICROS_PER_USD = 1_000_000n;

/** Micro-USD in one cent. Paddle amounts are whole cents. */
export const MICROS_PER_CENT = 10_000n;

/**
 * The largest whole-cent amount that fits a signed 64-bit integer, in
 * micro-USD (the largest `int64` is 9223372036854775807, which is not a
 * whole number of cents). Every amount a user enters is sent as an `int64`
 * `*_usd_micros` field, so an entered amount above this value cannot be
 * sent (docs/repository-billing-contract.md).
 */
export const MAX_WHOLE_CENT_INT64_USD_MICROS = 9_223_372_036_854_770_000n;

/** Converts whole USD to micro-USD. */
export function usd(whole: bigint): bigint {
  return whole * MICROS_PER_USD;
}

/** Why a typed amount was rejected. */
export enum AmountError {
  Empty = "empty",
  Invalid = "invalid",
  TooPrecise = "too-precise",
  NotWholeCents = "not-whole-cents",
}

export type ParsedAmount =
  { ok: true; micros: bigint } | { ok: false; error: AmountError };

/**
 * Parses a non-negative USD amount that a user typed, for example `25`,
 * `25.5`, `$1,000.00` or `0.000001`, into micro-USD. The text may have a
 * leading `$`, thousands separators in groups of three and at most six
 * decimals. Negative values are not accepted: every amount a user enters is
 * zero or more.
 */
export function parseUsd(text: string): ParsedAmount {
  let value = text.trim();
  if (value.startsWith("$")) {
    value = value.slice(1).trim();
  }
  if (value === "") {
    return { ok: false, error: AmountError.Empty };
  }
  const match = /^(\d{1,3}(?:,\d{3})+|\d+)?(?:\.(\d*))?$/.exec(value);
  if (match === null || (match[1] === undefined && !match[2])) {
    return { ok: false, error: AmountError.Invalid };
  }
  const whole = (match[1] ?? "0").replaceAll(",", "");
  const fraction = match[2] ?? "";
  if (fraction.length > 6) {
    return { ok: false, error: AmountError.TooPrecise };
  }
  return {
    ok: true,
    micros: BigInt(whole) * MICROS_PER_USD + BigInt(fraction.padEnd(6, "0")),
  };
}

/** Parses an amount that must be a whole number of cents. */
export function parseUsdCents(text: string): ParsedAmount {
  const parsed = parseUsd(text);
  if (parsed.ok && parsed.micros % MICROS_PER_CENT !== 0n) {
    return { ok: false, error: AmountError.NotWholeCents };
  }
  return parsed;
}

/** The message of a parse error, for a field error. */
export function amountErrorMessage(error: AmountError): string {
  switch (error) {
    case AmountError.Empty:
      return "Enter an amount.";
    case AmountError.Invalid:
      return "Enter an amount in USD, for example 25.00.";
    case AmountError.TooPrecise:
      return "Use at most 6 decimal places.";
    case AmountError.NotWholeCents:
      return "Use whole cents (at most 2 decimal places).";
  }
}

/**
 * Divides with rounding half away from zero. Used only to round a value for
 * display.
 */
function divRound(value: bigint, divisor: bigint): bigint {
  const negative = value < 0n;
  const abs = negative ? -value : value;
  const rounded = (abs + divisor / 2n) / divisor;
  return negative ? -rounded : rounded;
}

function groupThousands(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** Removes trailing zeros from a digit string. */
function trimTrailingZeros(digits: string): string {
  let end = digits.length;
  while (end > 0 && digits[end - 1] === "0") {
    end -= 1;
  }
  return digits.slice(0, end);
}

/**
 * Formats micro-USD in USD with `decimals` decimals (0 to 6), rounding half
 * away from zero, for example `$1,234.57` or `-$5.00`.
 */
function formatDecimals(micros: bigint, decimals: number): string {
  const scale = 10n ** BigInt(6 - decimals);
  const scaled = divRound(micros, scale);
  const negative = scaled < 0n;
  const abs = negative ? -scaled : scaled;
  const unit = 10n ** BigInt(decimals);
  const whole = groupThousands((abs / unit).toString());
  const fraction =
    decimals === 0 ? "" : `.${(abs % unit).toString().padStart(decimals, "0")}`;
  return `${negative ? "-" : ""}$${whole}${fraction}`;
}

/**
 * Formats an amount in USD with 2 decimals, for example `$25.00`. Values
 * between cents round half away from zero, for display only.
 */
export function formatUsd(micros: bigint): string {
  return formatDecimals(micros, 2);
}

/**
 * Formats a usage cost. A nonzero cost below $0.01 (in absolute value) shows
 * with up to 6 decimals without trailing zeros (at least 2), for example
 * `$0.000123` or `$0.005`; every other cost shows with 2 decimals like
 * `formatUsd`.
 */
export function formatUsageCost(micros: bigint): string {
  const abs = micros < 0n ? -micros : micros;
  if (abs === 0n || abs >= MICROS_PER_CENT) {
    return formatUsd(micros);
  }
  const digits = trimTrailingZeros(abs.toString().padStart(6, "0")).padEnd(
    2,
    "0",
  );
  return `${micros < 0n ? "-" : ""}$0.${digits}`;
}

/**
 * The plain decimal text of an amount for an input field: 2 decimals for
 * whole cents and up to 6 otherwise, for example `25.00`.
 */
export function toAmountInput(micros: bigint): string {
  const negative = micros < 0n;
  const abs = negative ? -micros : micros;
  const whole = (abs / MICROS_PER_USD).toString();
  const fraction = trimTrailingZeros(
    (abs % MICROS_PER_USD).toString().padStart(6, "0"),
  ).padEnd(2, "0");
  return `${negative ? "-" : ""}${whole}.${fraction}`;
}
