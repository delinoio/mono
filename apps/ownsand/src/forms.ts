// SPDX-License-Identifier: Apache-2.0

/**
 * Client-side checks of the create sandbox form. They mirror the rules of
 * the protos contract so that the user sees the problem before the request;
 * the control plane checks them again.
 */

import { create } from "@bufbuild/protobuf";
import {
  type DeviceLimitsInput,
  DeviceLimitsInputSchema,
} from "ownsand-sdk/gen/common_pb";

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

const utf8 = new TextEncoder();

/**
 * Parses labels written as one `key=value` pair per line. Labels must not
 * contain secrets: every member and API key can read them.
 */
export function parseLabels(
  text: string,
): { labels: Record<string, string> } | { error: string } {
  const labels: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (line === "") continue;
    const at = line.indexOf("=");
    if (at <= 0)
      return { error: `Write each label as key=value: "${line.slice(0, 40)}"` };
    const key = line.slice(0, at).trim();
    const value = line.slice(at + 1).trim();
    if (utf8.encode(key).length > 63)
      return { error: "A label key has at most 63 bytes." };
    if (utf8.encode(value).length > 255)
      return { error: "A label value has at most 255 bytes." };
    labels[key] = value;
  }
  if (Object.keys(labels).length > MAX_LABELS) {
    return { error: `Use at most ${MAX_LABELS} labels.` };
  }
  return { labels };
}

/** The limit fields of the device form; `null` is a blank input. */
export interface DeviceLimitsForm {
  vcpu: number | null;
  memoryMib: number | null;
  diskGib: number | null;
  overcommitPercent: number;
}

/**
 * Builds the ReplaceDeviceLimits input of the device form. The RPC needs
 * every value, and a blank input is not zero: it returns the message to show
 * for the first blank field, so the form never sends a silent zero. An
 * explicit zero is a value.
 */
export function deviceLimitsInput(
  form: DeviceLimitsForm,
): { limits: DeviceLimitsInput } | { error: string } {
  const blank = [
    [form.vcpu, "vCPU limit"],
    [form.memoryMib, "Memory limit"],
    [form.diskGib, "Disk limit"],
  ].find(([value]) => value === null);
  if (blank !== undefined) {
    return { error: `${blank[1]} is required. Enter 0 to allow none.` };
  }
  return {
    limits: create(DeviceLimitsInputSchema, {
      vcpuCount: form.vcpu!,
      memoryMib: BigInt(form.memoryMib!),
      diskGib: BigInt(form.diskGib!),
      vcpuOvercommitPercent: form.overcommitPercent,
    }),
  };
}
