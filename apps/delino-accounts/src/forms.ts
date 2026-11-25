// SPDX-License-Identifier: Apache-2.0
import {
  amountErrorMessage,
  formatUsd,
  MAX_WHOLE_CENT_INT64_USD_MICROS,
  parseUsdCents,
  usd,
} from "./money.ts";

/** A field value that passed validation, or the message of the field error. */
export type Checked<T> = { ok: true; value: T } | { ok: false; error: string };

/** Upper limit of a billing account name, in Unicode code points. */
export const MAX_ACCOUNT_NAME_LENGTH = 100;

/**
 * The white space that Go's `strings.TrimSpace` removes (`unicode.IsSpace`):
 * U+0009 to U+000D, U+0020, U+0085, U+00A0, U+1680, U+2000 to U+200A,
 * U+2028, U+2029, U+202F, U+205F and U+3000. The API server trims the
 * billing account name with it. `String.prototype.trim` differs: it also
 * removes U+FEFF and does not remove U+0085, so the app cannot use it for
 * a value that the server trims. Keep this set equal to `unicode.IsSpace`.
 */
const GO_SPACE_CLASS =
  "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const GO_SPACE_EDGES = new RegExp(
  `^[${GO_SPACE_CLASS}]+|[${GO_SPACE_CLASS}]+$`,
  "g",
);

/** Removes the white space at the start and the end like `strings.TrimSpace`. */
function trimGoSpace(input: string): string {
  return input.replace(GO_SPACE_EDGES, "");
}

/**
 * Checks a billing account name like the server: 1 to 100 Unicode code
 * points after the white space at the start and the end is removed (the set
 * of Go's `strings.TrimSpace`, see `trimGoSpace`), and no control
 * characters.
 */
export function checkAccountName(input: string): Checked<string> {
  const name = trimGoSpace(input);
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

// Go's net/mail allows RFC 5322 dot-atoms, including non-ASCII Unicode
// scalar values, in both parts of a plain address.
const EMAIL_ATOM =
  /^[a-z0-9!#$%&'*+/=?^`_{|}~\u0080-\uD7FF\uE000-\u{10FFFF}-]+$/iu;
const IPV4_ADDRESS = /^(?:0|[1-9][0-9]{0,2})(?:\.(?:0|[1-9][0-9]{0,2})){3}$/;
const IPV6_CHARACTERS = /^[0-9a-f:.]+$/i;

function isEmailDotAtom(value: string): boolean {
  return value.split(".").every((atom) => EMAIL_ATOM.test(atom));
}

function isEmailDomain(domain: string): boolean {
  if (isEmailDotAtom(domain)) return true;
  if (!domain.startsWith("[") || !domain.endsWith("]")) return false;

  // The Go version in go.mod parses the bracket contents as a bare IP address.
  const address = domain.slice(1, -1);
  if (IPV4_ADDRESS.test(address)) {
    return address.split(".").every((octet) => Number(octet) <= 255);
  }
  if (!IPV6_CHARACTERS.test(address)) return false;

  try {
    new URL(`http://[${address}]/`);
    return true;
  } catch {
    return false;
  }
}

/** Checks an invitation email address (`local@domain`, at most 254 bytes). */
export function checkEmail(input: string): Checked<string> {
  const email = trimGoSpace(input);
  if (email === "") {
    return { ok: false, error: "Enter an email address." };
  }
  const [local = "", domain = "", extra] = email.split("@", 3);
  // Pasted angle quotes can wrap an address. Go treats these Unicode
  // characters as part of the address and would send to a different mailbox.
  const hasAngleQuotes = email.startsWith("‹") && email.endsWith("›");
  if (
    new TextEncoder().encode(email).length > MAX_EMAIL_BYTES ||
    extra !== undefined ||
    hasAngleQuotes ||
    !isEmailDotAtom(local) ||
    !isEmailDomain(domain)
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
 * comparison is between `bigint` micro-USD values. An amount never exceeds
 * `MAX_WHOLE_CENT_INT64_USD_MICROS`, the largest value that the API field
 * (`int64`) can carry; a smaller `limits.max` takes precedence.
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
  const max =
    limits.max !== undefined && limits.max < MAX_WHOLE_CENT_INT64_USD_MICROS
      ? limits.max
      : MAX_WHOLE_CENT_INT64_USD_MICROS;
  if (parsed.micros > max) {
    return {
      ok: false,
      error: `Enter at most ${formatUsd(max)}.`,
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
