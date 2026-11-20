// SPDX-License-Identifier: Apache-2.0
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";

/** Formats a protobuf timestamp in the user's locale, or "—" when absent. */
export function formatTime(value: Timestamp | undefined): string {
  if (value === undefined) {
    return "—";
  }
  return timestampDate(value).toLocaleString();
}

/** CPU, memory and disk amounts (`Resources` and `DeviceLimits`). */
export interface ResourceAmounts {
  readonly vcpuCount: bigint;
  readonly memoryMib: bigint;
  readonly diskGib: bigint;
}

/** Formats CPU, memory and disk amounts. */
export function formatResources(value: ResourceAmounts | undefined): string {
  if (value === undefined) {
    return "—";
  }
  return `${value.vcpuCount} vCPU · ${value.memoryMib} MiB · ${value.diskGib} GiB`;
}

/**
 * Formats an enum value by its generated name: `HOST_OS_LINUX`-style names
 * are generated without the prefix (`LINUX`), so this gives "Linux".
 */
export function formatEnum(
  enumObject: Record<string, string | number>,
  value: number,
): string {
  const name = enumObject[value];
  if (typeof name !== "string" || name === "UNSPECIFIED") {
    return "Unknown";
  }
  const words = name.toLowerCase().split("_");
  return words
    .map((word, index) =>
      index === 0 ? word.charAt(0).toUpperCase() + word.slice(1) : word,
    )
    .join(" ");
}
