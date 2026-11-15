// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  DEFAULT_ACCOUNTS_API_ORIGIN,
  readAccountsApiOrigin,
  resolveAccountsApiOrigin,
} from "./config.ts";

describe("readAccountsApiOrigin", () => {
  test("reads PUBLIC_API_ORIGIN from the given environment", () => {
    expect(
      readAccountsApiOrigin({ PUBLIC_API_ORIGIN: "http://127.0.0.1:8100/" }),
    ).toBe("http://127.0.0.1:8100");
  });

  test("uses the production origin when the variable is absent", () => {
    expect(readAccountsApiOrigin({})).toBe(DEFAULT_ACCOUNTS_API_ORIGIN);
  });

  test("rejects an invalid value with the resolver error", () => {
    expect(() =>
      readAccountsApiOrigin({
        PUBLIC_API_ORIGIN: "ftp://accounts.api.delino.io",
      }),
    ).toThrow("PUBLIC_API_ORIGIN must use http or https");
  });
});

describe("resolveAccountsApiOrigin", () => {
  test("uses the production origin when the value is unset or empty", () => {
    expect(resolveAccountsApiOrigin(undefined)).toBe(
      DEFAULT_ACCOUNTS_API_ORIGIN,
    );
    expect(resolveAccountsApiOrigin("  ")).toBe(DEFAULT_ACCOUNTS_API_ORIGIN);
  });

  test("normalizes an explicit origin", () => {
    expect(resolveAccountsApiOrigin("http://127.0.0.1:8100/")).toBe(
      "http://127.0.0.1:8100",
    );
    expect(resolveAccountsApiOrigin("https://ACCOUNTS.api.delino.io")).toBe(
      "https://accounts.api.delino.io",
    );
  });

  test.each([
    "not a url",
    "ftp://accounts.api.delino.io",
    "https://accounts.api.delino.io/v1",
    "https://accounts.api.delino.io/?debug=1",
    "https://accounts.api.delino.io/#top",
    "https://accounts.api.delino.io?",
    "https://accounts.api.delino.io#",
    "https://accounts.api.delino.io/?",
    "https://accounts.api.delino.io/#",
    "https://accounts.api.delino.io?#",
    "https://accounts.api.delino.io/?#",
    // Paths that the URL parser normalizes to the root.
    "https://accounts.api.delino.io/..",
    "https://accounts.api.delino.io/.",
    "https://accounts.api.delino.io/./",
    "https://accounts.api.delino.io/%2e",
    "https://accounts.api.delino.io/%2E%2e",
    "https://accounts.api.delino.io/a/..",
    "https://accounts.api.delino.io\\..",
    "https://accounts.api.delino.io\\",
    "https://accounts.api.delino.io//",
    "https://accounts.api.delino.io/\t..",
  ])("rejects %s", (value) => {
    expect(() => resolveAccountsApiOrigin(value)).toThrow("PUBLIC_API_ORIGIN");
  });

  test("keeps the path-without-root message for a path that normalizes to the root", () => {
    expect(() =>
      resolveAccountsApiOrigin("https://accounts.api.delino.io/.."),
    ).toThrow("must be an origin without a path, query or fragment");
  });

  test.each([
    ["https://accounts.api.delino.io/", "https://accounts.api.delino.io"],
    ["http://127.0.0.1:8100/", "http://127.0.0.1:8100"],
    [
      "https://accounts.api.delino.io:8443",
      "https://accounts.api.delino.io:8443",
    ],
    ["https://accounts.api.delino.io/\t", "https://accounts.api.delino.io"],
    // Lenient forms that the URL parser accepts stay accepted.
    ["https:accounts.api.delino.io", "https://accounts.api.delino.io"],
    ["https:///accounts.api.delino.io", "https://accounts.api.delino.io"],
  ])("accepts %s", (value, origin) => {
    expect(resolveAccountsApiOrigin(value)).toBe(origin);
  });

  test("names the scheme of a real non-http scheme", () => {
    expect(() =>
      resolveAccountsApiOrigin("ftp://accounts.api.delino.io"),
    ).toThrow("PUBLIC_API_ORIGIN must use http or https, got ftp:");
  });

  test.each([
    "token123:secret@host",
    "abc123:pass@host",
    "TOKEN123:secret@host",
    "accounts.api.delino.io:8100",
  ])("does not echo the text before the first colon of %s", (value) => {
    let message = "";
    try {
      resolveAccountsApiOrigin(value);
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toContain("PUBLIC_API_ORIGIN must use http or https");
    expect(message).not.toMatch(/token123|abc123|secret|pass|accounts|8100/i);
    expect(message).not.toContain("got");
  });

  test("keeps the credential check before the path check", () => {
    expect(() =>
      resolveAccountsApiOrigin("https://user:pass@accounts.api.delino.io/.."),
    ).toThrow("PUBLIC_API_ORIGIN must not contain credentials");
  });

  test("rejects a path that normalizes to the root through the build reader", () => {
    expect(() =>
      readAccountsApiOrigin({
        PUBLIC_API_ORIGIN: "https://accounts.api.delino.io/..",
      }),
    ).toThrow("PUBLIC_API_ORIGIN must be an origin without a path");
  });

  test.each([
    "https://user:pass@accounts.api.delino.io",
    "https://user@accounts.api.delino.io",
    "https://:pass@accounts.api.delino.io",
    "http://user:pass@127.0.0.1:8100/",
    "https://user:pass@accounts.api.delino.io/v1",
    "https://user:pass@accounts.api.delino.io/?debug=1#top",
    "ftp://user:pass@accounts.api.delino.io",
  ])("rejects credentials in %s without echoing them", (value) => {
    let message = "";
    try {
      resolveAccountsApiOrigin(value);
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toContain("PUBLIC_API_ORIGIN");
    expect(message).toContain("credentials");
    expect(message).not.toContain("pass");
    expect(message).not.toContain("user");
  });

  test.each([
    "https://user:pass@",
    "https://user:pass@acc ounts.api.delino.io",
    "user:pass@accounts.api.delino.io",
    "ftp://accounts.api.delino.io/secret-path?token=pass",
    "https://accounts.api.delino.io/secret-path?token=pass#pass",
  ])("does not echo the raw value of rejected input %s", (value) => {
    let message = "";
    try {
      resolveAccountsApiOrigin(value);
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toContain("PUBLIC_API_ORIGIN");
    expect(message).not.toContain("pass");
    expect(message).not.toContain("user");
    expect(message).not.toContain("secret-path");
  });
});
