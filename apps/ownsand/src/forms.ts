// SPDX-License-Identifier: Apache-2.0

/**
 * Client-side checks of the create sandbox form (the TTLs and the resource
 * ranges). They mirror the rules of the protos contract so that the user sees
 * the problem before the request; the control plane checks them again.
 */

import { create } from "@bufbuild/protobuf";
import {
  type DeviceLimitsInput,
  DeviceLimitsInputSchema,
} from "@delino/ownsand-protos/v1/common_pb";
import { MAX_INT32 } from "./format.ts";

export const MAX_TTL_SECONDS = 7 * 24 * 3600;
export const MAX_LABELS = 32;

export interface TtlInput {
  idleMinutes: number | null;
  maxHours: number | null;
}

/** Returns field errors of the TTL inputs, or an empty object. */
export function ttlErrors(input: TtlInput): { idle?: string; max?: string } {
  const errors: { idle?: string; max?: string } = {};
  if (input.idleMinutes === null || !(input.idleMinutes > 0)) {
    errors.idle = "Enter an idle TTL greater than zero.";
  }
  if (input.maxHours === null || !(input.maxHours > 0)) {
    errors.max = "Enter a maximum TTL greater than zero.";
  } else if (input.maxHours * 3600 > MAX_TTL_SECONDS) {
    errors.max = "The maximum TTL is at most 7 days (168 hours).";
  }
  if (
    errors.idle === undefined &&
    errors.max === undefined &&
    input.idleMinutes! * 60 > input.maxHours! * 3600
  ) {
    errors.idle = "The idle TTL must not be longer than the maximum TTL.";
  }
  return errors;
}

export interface ResourceInput {
  vcpu: number | null;
  memory: number | null;
  disk: number | null;
}

/**
 * The largest memory or disk value that the form sends. The form holds a
 * `number`, and a value above 2^53 - 1 would be rounded when it becomes the
 * `int64` of the request.
 */
export const MAX_RESOURCE_INT64 = Number.MAX_SAFE_INTEGER;

/**
 * Returns field errors of the resource inputs, or an empty object. The vCPU
 * count is an `int32` and memory and disk are `int64` values in the request;
 * a value outside the range would fail in the protobuf serializer, or be
 * rounded, so the form reports it before the request.
 */
export function resourceErrors(input: ResourceInput): {
  vcpu?: string;
  memory?: string;
  disk?: string;
} {
  const errors: { vcpu?: string; memory?: string; disk?: string } = {};
  if (input.vcpu === null || !(input.vcpu > 0)) {
    errors.vcpu = "Enter at least 1 vCPU.";
  } else if (!Number.isInteger(input.vcpu) || input.vcpu > MAX_INT32) {
    errors.vcpu = `Enter a whole number of vCPUs from 1 to ${MAX_INT32}.`;
  }
  if (input.memory === null || !(input.memory > 0)) {
    errors.memory = "Enter the memory in MiB.";
  } else if (
    !Number.isInteger(input.memory) ||
    input.memory > MAX_RESOURCE_INT64
  ) {
    errors.memory = `Enter a whole number of MiB from 1 to ${MAX_RESOURCE_INT64}.`;
  }
  if (input.disk === null || !(input.disk > 0)) {
    errors.disk = "Enter the disk size in GiB.";
  } else if (!Number.isInteger(input.disk) || input.disk > MAX_RESOURCE_INT64) {
    errors.disk = `Enter a whole number of GiB from 1 to ${MAX_RESOURCE_INT64}.`;
  }
  return errors;
}

const utf8 = new TextEncoder();

/** U+0000, which `go/ownsand/validate` rejects in a label key or value. */
const NUL = "\u0000";

/**
 * Parses labels written as one `key=value` pair per line. Labels must not
 * contain secrets: every member and API key can read them.
 */
export function parseLabels(
  text: string,
): { labels: Record<string, string> } | { error: string } {
  // A Map keeps every key; assigning `labels["__proto__"]` on a plain object
  // would call the prototype setter and silently drop the label.
  const labels = new Map<string, string>();
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (line === "") continue;
    const at = line.indexOf("=");
    if (at <= 0)
      return {
        // A NUL character in the echo would make the message unreadable.
        error: `Write each label as key=value: "${line.slice(0, 40).replaceAll(NUL, "")}"`,
      };
    const key = line.slice(0, at).trim();
    const value = line.slice(at + 1).trim();
    // The control plane rejects U+0000 in a label because PostgreSQL cannot
    // store it, so the form reports it before it sends the request.
    if (key.includes(NUL))
      return { error: "A label key must not contain a NUL character." };
    if (value.includes(NUL))
      return { error: "A label value must not contain a NUL character." };
    if (utf8.encode(key).length > 63)
      return { error: "A label key has at most 63 bytes." };
    if (utf8.encode(value).length > 255)
      return { error: "A label value has at most 255 bytes." };
    labels.set(key, value);
  }
  if (labels.size > MAX_LABELS) {
    return { error: `Use at most ${MAX_LABELS} labels.` };
  }
  // `Object.fromEntries` defines own data properties, including `__proto__`.
  return { labels: Object.fromEntries(labels) };
}

/** The largest value of an `int64` device limit: 2^63 - 1. */
export const MAX_INT64 = 9223372036854775807n;

/** Parses a non-negative whole number of the `int64` range without rounding. */
export function parseInt64Limit(
  text: string,
): { value: bigint } | { error: "blank" | "invalid" } {
  const trimmed = text.trim();
  if (trimmed === "") return { error: "blank" };
  // Digits only: a sign, a decimal point, an exponent or a separator is not a
  // whole number. `Number` would round above 2^53, so the digits go straight
  // to `BigInt`.
  if (!/^\d+$/.test(trimmed)) return { error: "invalid" };
  const value = BigInt(trimmed);
  if (value > MAX_INT64) return { error: "invalid" };
  return { value };
}

/** The limit fields of the device form that can carry an error. */
export enum DeviceLimitField {
  Vcpu = "vcpu",
  Memory = "memory",
  Disk = "disk",
}

/**
 * The limit fields of the device form. A blank vCPU input is `null`. Memory
 * and disk are the text of their inputs: an `int64` limit above 2^53 is not
 * exact as a `number`, so the form keeps the digits and parses them on save.
 */
export interface DeviceLimitsForm {
  vcpu: number | null;
  memoryMib: string;
  diskGib: string;
  overcommitPercent: number;
}

/**
 * Builds the ReplaceDeviceLimits input of the device form. The RPC needs
 * every value, and a blank input is not zero: it returns the message to show
 * for the first blank field, so the form never sends a silent zero. An
 * explicit zero is a value. A memory or disk limit must be a whole number
 * from 0 to 9223372036854775807 and keeps its exact value.
 */
export function deviceLimitsInput(
  form: DeviceLimitsForm,
): { limits: DeviceLimitsInput } | { error: string; field: DeviceLimitField } {
  const memory = parseInt64Limit(form.memoryMib);
  const disk = parseInt64Limit(form.diskGib);
  const blank: [DeviceLimitField, string] | undefined =
    form.vcpu === null
      ? [DeviceLimitField.Vcpu, "vCPU limit"]
      : "error" in memory && memory.error === "blank"
        ? [DeviceLimitField.Memory, "Memory limit"]
        : "error" in disk && disk.error === "blank"
          ? [DeviceLimitField.Disk, "Disk limit"]
          : undefined;
  if (blank !== undefined) {
    return {
      error: `${blank[1]} is required. Enter 0 to allow none.`,
      field: blank[0],
    };
  }
  if ("error" in memory) {
    return {
      error: `Memory limit must be a whole number from 0 to ${MAX_INT64}.`,
      field: DeviceLimitField.Memory,
    };
  }
  if ("error" in disk) {
    return {
      error: `Disk limit must be a whole number from 0 to ${MAX_INT64}.`,
      field: DeviceLimitField.Disk,
    };
  }
  return {
    limits: create(DeviceLimitsInputSchema, {
      vcpuCount: form.vcpu!,
      memoryMib: memory.value,
      diskGib: disk.value,
      vcpuOvercommitPercent: form.overcommitPercent,
    }),
  };
}
