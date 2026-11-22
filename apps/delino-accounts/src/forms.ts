// SPDX-License-Identifier: Apache-2.0
import { amountErrorMessage, formatUsd, parseUsdCents, usd } from "./money.ts";

/** A field value that passed validation, or the message of the field error. */
export type Checked<T> = { ok: true; value: T } | { ok: false; error: string };

/** Upper limit of a billing account name, in Unicode code points. */
export const MAX_ACCOUNT_NAME_LENGTH = 100;

/**
 * Checks a billing account name like the server: 1 to 100 Unicode code
 * points after white space at the start and the end is removed, and no
 * control characters.
 */
export function checkAccountName(input: string): Checked<string> {
  const name = input.trim();
  if (name === "") {
    return { ok: false, error: "Enter a name." };
  }
  if ([...name].length > MAX_ACCOUNT_NAME_LENGTH) {
    return {
      ok: false,
      error: `Use at most ${MAX_ACCOUNT_NAME_LENGTH} characters.`,
    };
  }
  if (/\p{Cc}/u.test(name)) {
    return { ok: false, error: "Remove control characters from the name." };
  }
  return { ok: true, value: name };
}

/** Upper limit of an email address in bytes (UTF-8). */
const MAX_EMAIL_BYTES = 254;

/** Checks an invitation email address (`local@domain`, at most 254 bytes). */
export function checkEmail(input: string): Checked<string> {
  const email = input.trim();
  if (email === "") {
    return { ok: false, error: "Enter an email address." };
  }
  if (
    new TextEncoder().encode(email).length > MAX_EMAIL_BYTES ||
    !/^[^\s@]+@[^\s@]+$/.test(email)
  ) {
    return {
      ok: false,
      error: "Enter an email address like name@example.com.",
    };
  }
  return { ok: true, value: email };
}

/** Limits of a manual top-up. */
export const TOP_UP_MIN_USD_MICROS = usd(25n);
export const TOP_UP_MAX_USD_MICROS = usd(10_000n);

/** Minimums of the automatic top-up settings. */
export const AUTO_TOP_UP_MIN_THRESHOLD_USD_MICROS = usd(10n);
export const AUTO_TOP_UP_MIN_AMOUNT_USD_MICROS = usd(25n);

/** Default low-balance alert threshold. */
export const DEFAULT_ALERT_THRESHOLD_USD_MICROS = usd(10n);

/**
 * Checks an amount in whole cents with optional limits (inclusive). Every
 * comparison is between `bigint` micro-USD values.
 */
export function checkAmount(
  input: string,
  limits: { min?: bigint; max?: bigint } = {},
): Checked<bigint> {
  const parsed = parseUsdCents(input);
  if (!parsed.ok) {
    return { ok: false, error: amountErrorMessage(parsed.error) };
  }
  if (limits.min !== undefined && parsed.micros < limits.min) {
    return {
      ok: false,
      error: `Enter at least ${formatUsd(limits.min)}.`,
    };
  }
  if (limits.max !== undefined && parsed.micros > limits.max) {
    return {
      ok: false,
      error: `Enter at most ${formatUsd(limits.max)}.`,
    };
  }
  return { ok: true, value: parsed.micros };
}

/** Checks a manual top-up amount: $25 to $10,000 in whole cents. */
export function checkTopUpAmount(input: string): Checked<bigint> {
  return checkAmount(input, {
    min: TOP_UP_MIN_USD_MICROS,
    max: TOP_UP_MAX_USD_MICROS,
  });
}

export interface AutoTopUpInput {
  enabled: boolean;
  threshold: string;
  amount: string;
  monthlyCap: string;
}

export interface AutoTopUpValues {
  enabled: boolean;
  thresholdUsdMicros: bigint;
  amountUsdMicros: bigint;
  monthlyCapUsdMicros: bigint;
}

export type AutoTopUpErrors = Partial<
  Record<"threshold" | "amount" | "monthlyCap", string>
>;

/**
 * Checks the automatic top-up settings like the server. When enabled: a
 * threshold of at least $10, an amount from $25 to $10,000 and a required
 * monthly cap of at least the amount, all in whole cents. When disabled,
 * the values are kept for a later enable and must be empty (zero) or
 * non-negative whole cents.
 */
export function checkAutoTopUp(
  input: AutoTopUpInput,
):
  | { ok: true; value: AutoTopUpValues }
  | { ok: false; errors: AutoTopUpErrors } {
  const errors: AutoTopUpErrors = {};
  const read = (
    key: keyof AutoTopUpErrors,
    text: string,
    min: bigint | undefined,
    max?: bigint,
  ): bigint => {
    if (!input.enabled && text.trim() === "") return 0n;
    const checked = checkAmount(text, { min, max });
    if (!checked.ok) {
      errors[key] = checked.error;
      return 0n;
    }
    return checked.value;
  };
  const threshold = read(
    "threshold",
    input.threshold,
    input.enabled ? AUTO_TOP_UP_MIN_THRESHOLD_USD_MICROS : undefined,
  );
  const amount = read(
    "amount",
    input.amount,
    input.enabled ? AUTO_TOP_UP_MIN_AMOUNT_USD_MICROS : undefined,
    input.enabled ? TOP_UP_MAX_USD_MICROS : undefined,
  );
  const monthlyCap = read("monthlyCap", input.monthlyCap, undefined);
  if (
    input.enabled &&
    errors.monthlyCap === undefined &&
    errors.amount === undefined &&
    monthlyCap < amount
  ) {
    errors.monthlyCap = `Enter at least the top-up amount (${formatUsd(amount)}).`;
  }
  if (Object.keys(errors).length > 0) {
    return { ok: false, errors };
  }
  return {
    ok: true,
    value: {
      enabled: input.enabled,
      thresholdUsdMicros: threshold,
      amountUsdMicros: amount,
      monthlyCapUsdMicros: monthlyCap,
    },
  };
}

/** Checks a low-balance alert threshold: at least $0, whole cents. */
export function checkAlertThreshold(input: string): Checked<bigint> {
  return checkAmount(input, { min: 0n });
}
