// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { MarkerOffsetFinder } from "./marker-offset.ts";

const encoder = new TextEncoder();
const bytes = (text: string): Uint8Array => encoder.encode(text);

describe("MarkerOffsetFinder", () => {
  test("finds the marker in a chunk that also holds the next line", () => {
    const finder = new MarkerOffsetFinder("one\n");
    expect(finder.push(bytes("one\ntwo\n"))).toBe(4);
  });

  test("retains the byte offset of a marker split across chunks", () => {
    const finder = new MarkerOffsetFinder("one\n");
    expect(finder.push(bytes("é"))).toBeUndefined();
    expect(finder.push(bytes("on"))).toBeUndefined();
    expect(finder.push(bytes("e"))).toBeUndefined();
    expect(finder.push(bytes("\ntwo\n"))).toBe(6);
    expect(finder.push(bytes("one\n"))).toBe(6);
  });

  test("retains the first offset when later chunks lack or repeat the marker", () => {
    const finder = new MarkerOffsetFinder("one\n");
    expect(finder.push(bytes("one\n"))).toBe(4);
    expect(finder.push(bytes("two\n"))).toBe(4);
    expect(finder.push(bytes("one\n"))).toBe(4);
  });

  test("retains the first offset after an empty chunk", () => {
    const finder = new MarkerOffsetFinder("one\n");
    expect(finder.push(bytes("one\n"))).toBe(4);
    expect(finder.push(new Uint8Array(0))).toBe(4);
  });

  test("counts bytes of earlier chunks, not characters", () => {
    const finder = new MarkerOffsetFinder("one\n");
    // "é" is two bytes in UTF-8 and one UTF-16 code unit.
    expect(finder.push(bytes("é"))).toBeUndefined();
    expect(finder.push(bytes("one\n"))).toBe(6);
  });

  test("returns the first of several markers", () => {
    const finder = new MarkerOffsetFinder("one\n");
    expect(finder.push(bytes("x"))).toBeUndefined();
    expect(finder.push(bytes("one\none\n"))).toBe(5);
  });

  test("rejects an empty marker", () => {
    expect(() => new MarkerOffsetFinder("")).toThrow(RangeError);
  });
});
