// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import { OwnSandValidationError } from "./errors.ts";
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

/** Checks sandbox resources: all three are required and positive. */
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
  if (!(resources.vcpuCount > 0)) {
    throw new OwnSandValidationError(
      "resources.vcpu_count",
      "must be greater than zero",
    );
  }
  if (!(BigInt(resources.memoryMib) > 0n)) {
    throw new OwnSandValidationError(
      "resources.memory_mib",
      "must be greater than zero",
    );
  }
  if (!(BigInt(resources.diskGib) > 0n)) {
    throw new OwnSandValidationError(
      "resources.disk_gib",
      "must be greater than zero",
    );
  }
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
