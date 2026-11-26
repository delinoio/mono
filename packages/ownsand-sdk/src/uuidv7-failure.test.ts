// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { isUuidV7, UuidV7Generator, uuidV7TimestampMs } from "./uuidv7.ts";

// The largest value that the 48-bit UUID v7 timestamp field can hold.
const MAX_TIMESTAMP_MS = 2 ** 48 - 1;

describe("UuidV7Generator after a failed generation", () => {
  test("recovers on the same instance after an out-of-range clock reading", () => {
    let now = 1_700_000_000_000;
    const gen = new UuidV7Generator(() => now);
    const first = gen.next();

    now = 2 ** 48;
    expect(() => gen.next()).toThrow();
    // The failed call must not leave the out-of-range timestamp behind.
    expect(() => gen.next()).toThrow();

    now = 1_700_000_000_005;
    const recovered = gen.next();
    expect(isUuidV7(recovered)).toBe(true);
    expect(uuidV7TimestampMs(recovered)).toBe(1_700_000_000_005);
    expect(recovered > first).toBe(true);
  });

  test("recovers when the first reading is out of range", () => {
    let now = 2 ** 48;
    const gen = new UuidV7Generator(() => now);
    expect(() => gen.next()).toThrow();

    now = 1_700_000_000_000;
    const id = gen.next();
    expect(isUuidV7(id)).toBe(true);
    expect(uuidV7TimestampMs(id)).toBe(1_700_000_000_000);
  });

  test("keeps strict ordering across success, failure and success", () => {
    let now = 1_700_000_000_000;
    const gen = new UuidV7Generator(() => now);
    const ids: string[] = [gen.next(), gen.next()];

    now = Number.POSITIVE_INFINITY;
    expect(() => gen.next()).toThrow(RangeError);
    now = Number.NEGATIVE_INFINITY;
    expect(() => gen.next()).toThrow(RangeError);
    now = Number.NaN;
    expect(() => gen.next()).toThrow(RangeError);

    // The clock recovers to the same millisecond as before the failures, so
    // the counter must continue from the last successful value.
    now = 1_700_000_000_000;
    ids.push(gen.next(), gen.next());
    now = 1_700_000_000_001;
    ids.push(gen.next());

    for (let i = 1; i < ids.length; i++) {
      expect(isUuidV7(ids[i]!)).toBe(true);
      expect(ids[i]! > ids[i - 1]!).toBe(true);
    }
  });

  test("keeps the counter when a failure follows a success in the same millisecond", () => {
    let now = 1_700_000_000_000;
    const gen = new UuidV7Generator(() => now);
    const before = gen.next();

    now = 2 ** 48 + 10;
    expect(() => gen.next()).toThrow();

    now = 1_700_000_000_000;
    const after = gen.next();
    expect(uuidV7TimestampMs(after)).toBe(1_700_000_000_000);
    // A failed call must not advance the counter: the next value is exactly
    // one counter step after the last successful one.
    expect(counterOf(after)).toBe(counterOf(before) + 1);
  });

  test("keeps the previous state when the counter rollover overflows the timestamp", () => {
    // At the last representable millisecond the counter cannot roll over into
    // the next millisecond. Generation must keep failing without producing a
    // malformed or out-of-order value, and the last successful value stays the
    // greatest one issued.
    const gen = new UuidV7Generator(() => MAX_TIMESTAMP_MS);
    let last = gen.next();
    let failures = 0;
    for (let i = 0; i < 0x2000 && failures === 0; i++) {
      try {
        const id = gen.next();
        expect(id > last).toBe(true);
        last = id;
      } catch {
        failures++;
      }
    }
    expect(failures).toBe(1);
    expect(uuidV7TimestampMs(last)).toBe(MAX_TIMESTAMP_MS);
    expect(counterOf(last)).toBe(0xfff);

    // The state is still the last successful one, so every retry fails the
    // same way instead of drifting into an out-of-range timestamp.
    expect(() => gen.next()).toThrow();
    expect(() => gen.next()).toThrow();
  });
});

// Reads the 12-bit counter from rand_a of a UUID v7.
function counterOf(id: string): number {
  return Number.parseInt(id.slice(15, 18), 16);
}
