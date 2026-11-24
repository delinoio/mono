// SPDX-License-Identifier: Apache-2.0
import { expect, test } from "@rstest/core";
import { isUuidV7, UuidV7Generator } from "./index.ts";

const MAX_TIMESTAMP_MS = 2 ** 48 - 1;

test("rejects a first clock reading above the limit and recovers at zero", () => {
  let now = MAX_TIMESTAMP_MS + 1;
  const generator = new UuidV7Generator(() => now);
  expect(() => generator.next()).toThrow(RangeError);

  now = 0;
  const uuid = generator.next();
  expect(isUuidV7(uuid)).toBe(true);
  expect(uuid.slice(0, 13)).toBe("00000000-0000");
});

test("keeps the timestamp and counter after a temporary clock overflow", () => {
  let now = 1_700_000_000_000;
  const generator = new UuidV7Generator(() => now);
  const before = generator.next();

  now = MAX_TIMESTAMP_MS + 11;
  expect(() => generator.next()).toThrow(RangeError);

  now = 1_700_000_000_000.75;
  const recovered = generator.next();
  expect(recovered.slice(0, 13)).toBe(before.slice(0, 13));
  expect(counterOf(recovered)).toBe(counterOf(before) + 1);
  expect(recovered > before).toBe(true);

  now = 1_699_999_999_999;
  const backward = generator.next();
  expect(backward.slice(0, 13)).toBe(before.slice(0, 13));
  expect(counterOf(backward)).toBe(counterOf(recovered) + 1);
  expect(backward > recovered).toBe(true);
});

test("issues UUIDs in the last millisecond and rejects carry overflow on retries", () => {
  let now = MAX_TIMESTAMP_MS;
  const generator = new UuidV7Generator(() => now);
  exhaustCounter(generator, "ffffffff-ffff");

  expect(() => generator.next()).toThrow(RangeError);
  expect(() => generator.next()).toThrow(RangeError);
  now--;
  expect(() => generator.next()).toThrow(RangeError);
});

test("carries into the last millisecond with a lower-half counter", () => {
  const generator = new UuidV7Generator(() => MAX_TIMESTAMP_MS - 1);
  const previous = exhaustCounter(generator, "ffffffff-fffe");

  const carried = generator.next();
  expect(isUuidV7(carried)).toBe(true);
  expect(carried.slice(0, 13)).toBe("ffffffff-ffff");
  expect(counterOf(carried)).toBeLessThanOrEqual(0x7ff);
  expect(carried > previous).toBe(true);
});

function exhaustCounter(generator: UuidV7Generator, prefix: string): string {
  let previous = generator.next();
  expect(isUuidV7(previous)).toBe(true);
  expect(previous.slice(0, 13)).toBe(prefix);
  for (let counter = counterOf(previous) + 1; counter <= 0xfff; counter++) {
    const next = generator.next();
    expect(isUuidV7(next)).toBe(true);
    expect(next.slice(0, 13)).toBe(prefix);
    expect(counterOf(next)).toBe(counter);
    expect(next > previous).toBe(true);
    previous = next;
  }
  return previous;
}

function counterOf(uuid: string): number {
  return Number.parseInt(uuid.slice(15, 18), 16);
}
