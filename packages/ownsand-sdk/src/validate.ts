// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import { OwnSandValidationError } from "./errors.ts";
import { isUuidV7 } from "./uuidv7.ts";
import { ExecRequestSchema } from "@delino/ownsand-protos/v1/data_frame_pb";

/**
 * Request rules that the SDK checks before it sends a request. They equal
 * the shared Go `validate` package; the control plane and the agent check
 * them again.
 */

/** Largest encoding of an exec request's command, env and working directory. */
export const MAX_EXEC_FIELDS_SIZE = 256 << 10;
/** Longest sandbox TTL: 7 days. */
export const MAX_TTL_MS = 7 * 24 * 60 * 60 * 1000;
export const MAX_LABELS = 32;
export const MAX_LABEL_KEY_SIZE = 63;
export const MAX_LABEL_VALUE_SIZE = 255;

const utf8 = new TextEncoder();

/** Checks `idle_ttl` and `max_ttl` in milliseconds. Both are required. */
export function validateTtl(
  idleTtlMs: number | undefined,
  maxTtlMs: number | undefined,
): void {
  if (idleTtlMs === undefined || idleTtlMs === null) {
    throw new OwnSandValidationError("idle_ttl", "is required");
  }
  if (maxTtlMs === undefined || maxTtlMs === null) {
    throw new OwnSandValidationError("max_ttl", "is required");
  }
  if (!Number.isFinite(idleTtlMs) || idleTtlMs <= 0) {
    throw new OwnSandValidationError("idle_ttl", "must be greater than zero");
  }
  if (!Number.isFinite(maxTtlMs) || maxTtlMs <= 0) {
    throw new OwnSandValidationError("max_ttl", "must be greater than zero");
  }
  // Compare the values that go on the wire, so that a positive TTL that rounds
  // to zero nanoseconds is rejected and the checks match the sent durations.
  const idleNanos = nanosFromMs(idleTtlMs);
  const maxNanos = nanosFromMs(maxTtlMs);
  if (idleNanos <= 0n) {
    throw new OwnSandValidationError(
      "idle_ttl",
      "must be at least one nanosecond",
    );
  }
  if (maxNanos <= 0n) {
    throw new OwnSandValidationError(
      "max_ttl",
      "must be at least one nanosecond",
    );
  }
  if (maxNanos > BigInt(MAX_TTL_MS) * 1_000_000n) {
    throw new OwnSandValidationError("max_ttl", "must be at most 7 days");
  }
  if (idleNanos > maxNanos) {
    throw new OwnSandValidationError(
      "idle_ttl",
      "must not be greater than max_ttl",
    );
  }
}

/** Largest `vcpu_count`: the `int32` range of the protobuf field. */
const MAX_VCPU_COUNT = 2n ** 31n - 1n;
/** Largest `memory_mib` and `disk_gib`: the `int64` range of the protobuf fields. */
const MAX_INT64 = 2n ** 63n - 1n;
/** Longest protobuf Duration in seconds (`google.protobuf.Duration` range). */
const MAX_DURATION_SECONDS = 315_576_000_000n;

/**
 * Checks one resource value. The value must be a positive integer within the
 * range of its protobuf field. A number must be a safe integer, so a fraction,
 * NaN, an infinity and a number that has lost integer precision are rejected;
 * a bigint is compared with the range directly. The check never relies on
 * `BigInt()` throwing, and the error names the rule, never the value.
 */
function validateResource(
  field: string,
  value: number | bigint,
  max: bigint,
): void {
  const rule = `must be an integer from 1 to ${max}`;
  let integer: bigint;
  if (typeof value === "bigint") {
    integer = value;
  } else if (typeof value === "number" && Number.isSafeInteger(value)) {
    integer = BigInt(value);
  } else {
    throw new OwnSandValidationError(field, rule);
  }
  if (integer <= 0n) {
    throw new OwnSandValidationError(field, "must be greater than zero");
  }
  if (integer > max) {
    throw new OwnSandValidationError(field, rule);
  }
}

/**
 * Checks sandbox resources: all three are required, positive integers within
 * the range of their protobuf fields (`int32` for `vcpu_count`, `int64` for
 * `memory_mib` and `disk_gib`).
 */
export function validateResources(
  resources:
    | {
        vcpuCount: number;
        memoryMib: number | bigint;
        diskGib: number | bigint;
      }
    | undefined,
): void {
  if (resources === undefined) {
    throw new OwnSandValidationError("resources", "is required");
  }
  validateResource("resources.vcpu_count", resources.vcpuCount, MAX_VCPU_COUNT);
  validateResource("resources.memory_mib", resources.memoryMib, MAX_INT64);
  validateResource("resources.disk_gib", resources.diskGib, MAX_INT64);
}

/** Checks sandbox labels. */
export function validateLabels(
  labels: Readonly<Record<string, string>> | undefined,
): void {
  const entries = Object.entries(labels ?? {});
  if (entries.length > MAX_LABELS) {
    throw new OwnSandValidationError("labels", `at most ${MAX_LABELS} entries`);
  }
  for (const [key, value] of entries) {
    if (key === "") {
      throw new OwnSandValidationError("labels", "keys must not be empty");
    }
    if (utf8.encode(key).length > MAX_LABEL_KEY_SIZE) {
      throw new OwnSandValidationError(
        "labels",
        `keys must be at most ${MAX_LABEL_KEY_SIZE} bytes`,
      );
    }
    if (utf8.encode(value).length > MAX_LABEL_VALUE_SIZE) {
      throw new OwnSandValidationError(
        "labels",
        `values must be at most ${MAX_LABEL_VALUE_SIZE} bytes`,
      );
    }
    // U+0000 is valid UTF-8 and passes protobuf, but PostgreSQL text and jsonb
    // values cannot store it, so the control plane would fail the request
    // after the round trip. Mirrors the Go validator, in the same order; the
    // message names the rule and never the label.
    if (key.includes("\0")) {
      throw new OwnSandValidationError(
        "labels",
        "keys must not contain a NUL byte",
      );
    }
    if (value.includes("\0")) {
      throw new OwnSandValidationError(
        "labels",
        "values must not contain a NUL byte",
      );
    }
  }
}

/** Checks the command, env, working directory and timeout of an exec. */
export function validateExec(input: {
  command: readonly string[];
  env?: Readonly<Record<string, string>>;
  workingDirectory?: string;
  timeoutMs?: number;
}): void {
  if (input.command.length === 0 || input.command[0] === "") {
    throw new OwnSandValidationError(
      "command",
      "needs a program as its first element",
    );
  }
  const size = toBinary(
    ExecRequestSchema,
    create(ExecRequestSchema, {
      command: [...input.command],
      env: { ...input.env },
      workingDirectory: input.workingDirectory ?? "",
    }),
  ).length;
  if (size > MAX_EXEC_FIELDS_SIZE) {
    throw new OwnSandValidationError(
      "command",
      `command, env and working_directory encode to more than ${MAX_EXEC_FIELDS_SIZE} bytes`,
    );
  }
  if (
    input.timeoutMs !== undefined &&
    !(Number.isFinite(input.timeoutMs) && input.timeoutMs >= 0)
  ) {
    throw new OwnSandValidationError("timeout", "must not be negative");
  }
  // Zero means no timeout, but a positive value that rounds to zero
  // nanoseconds would silently mean the same, so reject it.
  if (
    input.timeoutMs !== undefined &&
    input.timeoutMs > 0 &&
    nanosFromMs(input.timeoutMs) <= 0n
  ) {
    throw new OwnSandValidationError(
      "timeout",
      "must be zero or at least one nanosecond",
    );
  }
  // The wire type is a protobuf Duration, which holds at most 315576000000
  // seconds; the Go validator rejects a larger timeout the same way.
  if (
    input.timeoutMs !== undefined &&
    nanosFromMs(input.timeoutMs) > MAX_DURATION_SECONDS * 1_000_000_000n
  ) {
    throw new OwnSandValidationError("timeout", "is not a valid duration");
  }
}

/**
 * Reports whether a non-empty path is absolute for either guest OS: a POSIX
 * path, a Windows drive path (`C:\` or `C:/`), a UNC path that starts with two
 * separators (mixed separators allowed) or a root local device path (`\??\`).
 * A single leading backslash (`\file.txt`) is root-relative on Windows, so it
 * is not absolute. Mirrors the Go validator.
 */
function isAbsoluteGuestPath(path: string): boolean {
  if (path[0] === "/") {
    return true;
  }
  if (path[0] === "\\") {
    return path[1] === "\\" || path[1] === "/" || path.startsWith("\\??\\");
  }
  return (
    path.length >= 3 &&
    /^[A-Za-z]$/.test(path[0]!) &&
    path[1] === ":" &&
    (path[2] === "\\" || path[2] === "/")
  );
}

/** Checks that a path is absolute for some guest OS. */
export function validateGuestPath(field: string, path: string): void {
  if (path === "") {
    throw new OwnSandValidationError(field, "is required");
  }
  if (!path.isWellFormed()) {
    throw new OwnSandValidationError(field, "must be valid UTF-8");
  }
  const absolute = isAbsoluteGuestPath(path);
  if (!absolute) {
    throw new OwnSandValidationError(field, "must be an absolute guest path");
  }
}

/**
 * Checks the ID of an exec to attach to: a canonical UUID v7 in lowercase.
 * The UUID v7 check accepts either hex case, so an uppercase spelling gets
 * the lowercase rule; two spellings of one UUID would be two different execs.
 * Mirrors the Go `validate.ExecID`.
 */
export function validateExecId(field: string, value: string): void {
  if (value === "") {
    throw new OwnSandValidationError(field, "is required");
  }
  if (!isUuidV7(value.toLowerCase())) {
    throw new OwnSandValidationError(field, "must be a canonical UUID v7");
  }
  if (value !== value.toLowerCase()) {
    throw new OwnSandValidationError(
      field,
      "must be a canonical lowercase UUID v7",
    );
  }
}

/**
 * Checks a stream or file offset: it must not be negative. Mirrors the Go
 * validator, which rejects a negative `int64` offset.
 */
export function validateOffset(field: string, value: bigint): void {
  if (value < 0n) {
    throw new OwnSandValidationError(field, "must not be negative");
  }
}

/** The largest file mode that a write may set (`07777`, the protobuf rule). */
const MAX_FILE_MODE = 0o7777;

/**
 * Checks the mode of a written file. It travels in `finish.mode` of the last
 * chunk, so without this check a bad mode is only rejected after the upload.
 * Mirrors the Go validator.
 */
export function validateFileMode(mode: number): void {
  if (!Number.isInteger(mode) || mode < 0 || mode > MAX_FILE_MODE) {
    throw new OwnSandValidationError(
      "finish.mode",
      "must be an integer from 0 to 07777",
    );
  }
}

/**
 * Converts milliseconds to a whole number of nanoseconds, rounding half up to
 * the nearest nanosecond. The integer and fractional millisecond parts are
 * converted separately so that large values do not lose precision.
 */
function nanosFromMs(ms: number): bigint {
  const whole = Math.floor(ms);
  const fraction = ms - whole;
  return BigInt(whole) * 1_000_000n + BigInt(Math.round(fraction * 1_000_000));
}

/**
 * Converts milliseconds to a protobuf Duration at nanosecond precision.
 * Fractions of a nanosecond round to the nearest nanosecond.
 */
export function durationFromMs(ms: number): { seconds: bigint; nanos: number } {
  const total = nanosFromMs(ms);
  return {
    seconds: total / 1_000_000_000n,
    nanos: Number(total % 1_000_000_000n),
  };
}
