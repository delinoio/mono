// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { DeviceLimitField, deviceLimitsInput } from "./forms.ts";

const base = {
  vcpu: 4,
  memoryMib: "8192",
  diskGib: "100",
  overcommitPercent: 150,
};

const VCPU_RANGE_ERROR = {
  error: "vCPU limit must be a whole number from 0 to 2147483647.",
  field: DeviceLimitField.Vcpu,
};

describe("deviceLimitsInput vCPU limit", () => {
  test("rejects a value above the int32 range, a decimal and a negative", () => {
    for (const vcpu of [2147483648, 99999999999, 1.5, -1, Number.NaN]) {
      expect(deviceLimitsInput({ ...base, vcpu })).toEqual(VCPU_RANGE_ERROR);
    }
  });

  test("accepts the bounds of the range", () => {
    for (const vcpu of [0, 2147483647]) {
      const result = deviceLimitsInput({ ...base, vcpu });
      expect("limits" in result).toBe(true);
      if (!("limits" in result)) return;
      expect(result.limits.vcpuCount).toBe(vcpu);
    }
  });

  test("a blank vCPU limit keeps the required message", () => {
    expect(deviceLimitsInput({ ...base, vcpu: null })).toEqual({
      error: "vCPU limit is required. Enter 0 to allow none.",
      field: DeviceLimitField.Vcpu,
    });
  });

  test("keeps the order: blank fields first, then vCPU, then memory and disk", () => {
    expect(
      deviceLimitsInput({ ...base, vcpu: 2147483648, memoryMib: "" }),
    ).toEqual({
      error: "Memory limit is required. Enter 0 to allow none.",
      field: DeviceLimitField.Memory,
    });
    expect(
      deviceLimitsInput({ ...base, vcpu: 2147483648, memoryMib: "-1" }),
    ).toEqual(VCPU_RANGE_ERROR);
    expect(deviceLimitsInput({ ...base, memoryMib: "-1" })).toEqual({
      error:
        "Memory limit must be a whole number from 0 to 9223372036854775807.",
      field: DeviceLimitField.Memory,
    });
  });
});
