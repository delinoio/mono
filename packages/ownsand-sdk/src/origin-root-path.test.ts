// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { OwnSandClient } from "./client.ts";
import { DEFAULT_API_ORIGIN, resolveApiOrigin } from "./connection.ts";
import { OwnSandConfigError } from "./errors.ts";

function construct(origin: string): OwnSandClient {
  let fetched = 0;
  return new OwnSandClient({
    origin,
    env: { OWNSAND_API_KEY: "osk_test" },
    fetch: () => {
      fetched++;
      throw new Error(`unexpected fetch ${fetched}`);
    },
  });
}

describe("origin path that normalizes to the root", () => {
  test.each([
    "https://example.test/subdir/..",
    "https://example.test/subdir/../",
    "https://example.test/.",
    "https://example.test/./",
    "https://example.test/a/b/../..",
    "https://example.test/%2e",
    "https://example.test:8443/subdir/..",
    "https://example.test//",
    "https://example.test\\subdir\\..",
    " https://example.test/subdir/..\n",
  ])("rejects %j without echoing it", (origin) => {
    let caught: unknown;
    try {
      construct(origin);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(OwnSandConfigError);
    expect((caught as Error).message).toBe(
      "origin must not have a path, query or fragment",
    );
    expect((caught as Error).message).not.toContain("example.test");
    expect(() => resolveApiOrigin(origin)).toThrow(OwnSandConfigError);
  });

  test("still rejects an ordinary path", () => {
    expect(() => construct("https://example.test/api")).toThrow(
      OwnSandConfigError,
    );
  });

  test.each([
    ["https://example.test", "https://example.test"],
    ["https://example.test/", "https://example.test"],
    ["http://127.0.0.1:18300", "http://127.0.0.1:18300"],
    ["http://127.0.0.1:18300/", "http://127.0.0.1:18300"],
    ["HTTPS://Example.test:443/", "https://example.test"],
    [" https://example.test/ ", "https://example.test"],
  ])("accepts %j", (origin, expected) => {
    expect(resolveApiOrigin(origin)).toBe(expected);
    expect(() => construct(origin)).not.toThrow();
  });

  test("keeps the default origin valid", () => {
    expect(resolveApiOrigin(undefined)).toBe(DEFAULT_API_ORIGIN);
    expect(resolveApiOrigin("")).toBe(DEFAULT_API_ORIGIN);
    expect(() => construct(DEFAULT_API_ORIGIN)).not.toThrow();
  });
});
