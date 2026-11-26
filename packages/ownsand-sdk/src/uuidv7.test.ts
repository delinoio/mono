// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  canonicalizeUuidV7,
  isUuidV7,
  UuidV7Generator,
  uuidV7TimestampMs,
} from "./uuidv7.ts";

describe("UuidV7Generator", () => {
  test("generates canonical UUID v7 values with the timestamp", () => {
    const gen = new UuidV7Generator(() => 1_700_000_000_123);
    const id = gen.next();
    expect(isUuidV7(id)).toBe(true);
    expect(uuidV7TimestampMs(id)).toBe(1_700_000_000_123);
  });

  test.each([-1, -10, -0.25])(
    "clamps a first reading of %s ms to zero",
    (nowMs) => {
      const id = new UuidV7Generator(() => nowMs).next();
      expect(isUuidV7(id)).toBe(true);
      expect(uuidV7TimestampMs(id)).toBe(0);
    },
  );

  test("is strictly increasing across the Unix epoch and a rollback", () => {
    let now = -1;
    const gen = new UuidV7Generator(() => now);
    const ids = [-1, -1, 0, 1, -1].map((reading) => {
      now = reading;
      return gen.next();
    });

    expect(ids.map(uuidV7TimestampMs)).toEqual([0, 0, 0, 1, 1]);
    for (let i = 1; i < ids.length; i++) {
      expect(ids[i]! > ids[i - 1]!).toBe(true);
    }
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

describe("canonicalizeUuidV7", () => {
  const lower = "0192f4b6-7c3a-7def-8abc-0123456789ab";

  test("lowercases a UUID v7 in either hex case", () => {
    expect(canonicalizeUuidV7(lower)).toBe(lower);
    expect(canonicalizeUuidV7(lower.toUpperCase())).toBe(lower);
  });

  test("returns every other value unchanged", () => {
    for (const value of [
      "",
      "Sandbox-A",
      "0192F4B6-7C3A-4DEF-8ABC-0123456789AB",
      "0192F4B6-7C3A-7DEF-CABC-0123456789AB",
      `${lower.toUpperCase()} `,
    ]) {
      expect(canonicalizeUuidV7(value)).toBe(value);
    }
  });
});
