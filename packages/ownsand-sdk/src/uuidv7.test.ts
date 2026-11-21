// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { isUuidV7, UuidV7Generator, uuidV7TimestampMs } from "./uuidv7.ts";

describe("UuidV7Generator", () => {
  test("generates canonical UUID v7 values with the timestamp", () => {
    const gen = new UuidV7Generator(() => 1_700_000_000_123);
    const id = gen.next();
    expect(isUuidV7(id)).toBe(true);
    expect(uuidV7TimestampMs(id)).toBe(1_700_000_000_123);
  });

  test("is strictly increasing within one millisecond and across a clock step back", () => {
    let now = 1_700_000_000_000;
    const gen = new UuidV7Generator(() => now);
    const ids: string[] = [];
    for (let i = 0; i < 5000; i++) {
      if (i === 2500) now -= 10;
      ids.push(gen.next());
    }
    for (let i = 1; i < ids.length; i++) {
      expect(ids[i]! > ids[i - 1]!).toBe(true);
    }
  });

  test("rejects other versions", () => {
    expect(isUuidV7("0192f4b6-7c3a-4def-8abc-0123456789ab")).toBe(false);
    expect(isUuidV7("0192f4b6-7c3a-7def-cabc-0123456789ab")).toBe(false);
  });
});
