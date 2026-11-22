// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  API_RESOURCE,
  DEFAULT_ACCOUNTS_API_ORIGIN,
  PaddleEnvironment,
  readAccountsApiOrigin,
  readAccountsConfig,
  REQUIRED_API_SCOPE,
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

  test.each([
    ["two spaces", "  "],
    ["a tab", "\t"],
    ["CR LF", "\r\n"],
    ["a vertical tab", "\u000b"],
    ["a form feed", "\u000c"],
    ["mixed ASCII white space", " \t\u000b\u000c "],
  ])("uses the production origin for %s", (_name, input) => {
    expect(resolveAccountsApiOrigin(input)).toBe(DEFAULT_ACCOUNTS_API_ORIGIN);
  });

  test("uses the production origin for an empty string", () => {
    expect(resolveAccountsApiOrigin("")).toBe(DEFAULT_ACCOUNTS_API_ORIGIN);
  });

  test.each([
    ["U+00A0 before", " https://accounts.api.delino.io"],
    ["U+00A0 after", "https://accounts.api.delino.io "],
    ["U+FEFF before", "﻿https://accounts.api.delino.io"],
    ["U+3000 before", "　https://accounts.api.delino.io"],
    ["U+2028 before", " https://accounts.api.delino.io"],
    ["U+2029 before", " https://accounts.api.delino.io"],
    ["U+1680 before", " https://accounts.api.delino.io"],
    ["U+202F before", " https://accounts.api.delino.io"],
    ["U+205F before", " https://accounts.api.delino.io"],
    ["U+2000 and U+200A before", "  https://accounts.api.delino.io"],
    ["U+00A0 alone", " "],
    ["U+FEFF alone", "﻿"],
    ["U+3000 alone", "　"],
    ["U+00A0 before a path", " https://accounts.api.delino.io/v1"],
    ["U+00A0 before credentials", " https://user:pw@host"],
  ])("rejects Unicode white space: %s", (_name, input) => {
    expect(() => resolveAccountsApiOrigin(input)).toThrow(
      "PUBLIC_API_ORIGIN is not a valid URL",
    );
  });

  test("still accepts ASCII edge characters that the parser ignores", () => {
    expect(resolveAccountsApiOrigin(" https://accounts.api.delino.io ")).toBe(
      DEFAULT_ACCOUNTS_API_ORIGIN,
    );
    expect(
      resolveAccountsApiOrigin("\u0001https://accounts.api.delino.io"),
    ).toBe(DEFAULT_ACCOUNTS_API_ORIGIN);
    expect(
      resolveAccountsApiOrigin("https://accounts.api.delino.io\u0000"),
    ).toBe(DEFAULT_ACCOUNTS_API_ORIGIN);
    expect(resolveAccountsApiOrigin("https://accounts.api.delino.io/\t")).toBe(
      DEFAULT_ACCOUNTS_API_ORIGIN,
    );
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

  test.each([
    "sk-proj-abc123://accounts.api.delino.io",
    "token123://host",
    "ftp://accounts.api.delino.io",
    "user:pass@host",
    "token123:secret@host",
    "abc123:pass@host",
    "TOKEN123:secret@host",
    "accounts.api.delino.io:8100",
  ])("reports only the scheme requirement for %s", (value) => {
    let message = "";
    try {
      resolveAccountsApiOrigin(value);
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toBe("PUBLIC_API_ORIGIN must use http or https");
  });

  // The URL parser removes leading and trailing C0 control characters and
  // spaces before parsing, so the raw-path check must ignore them as well.
  test.each([
    "\u0001https://accounts.api.delino.io",
    "\u0000https://accounts.api.delino.io/",
    " \u0001 https://accounts.api.delino.io",
    "https://accounts.api.delino.io/\u0000",
    "https://accounts.api.delino.io/\u0001\u0002",
    "https://accounts.api.delino.io\u0000",
    "https://accounts.api.delino.io\u001f",
    "https://accounts.api.delino.io \u0000 ",
  ])("accepts the origin surrounded by control characters in %j", (value) => {
    expect(resolveAccountsApiOrigin(value)).toBe(DEFAULT_ACCOUNTS_API_ORIGIN);
  });

  test.each([
    "https://accounts.api.delino.io/path\u0000",
    "https://accounts.api.delino.io/\u0000x",
    "https://accounts.api.delino.io?\u0000",
    "https://accounts.api.delino.io#\u0000",
    "\u0000https://accounts.api.delino.io/path",
    "\u0000https://accounts.api.delino.io/..",
    "https://accounts.api.delino.io/..\u0000",
    "https://accounts.api.delino.io/path/dir\u0001",
  ])("still rejects a path, query or fragment in %j", (value) => {
    expect(() => resolveAccountsApiOrigin(value)).toThrow(
      "PUBLIC_API_ORIGIN must be an origin without a path, query or fragment",
    );
  });

  test.each([
    "\u0001",
    "\u0000\u0000",
    "https://accounts.api.delino.io\u0000/path",
  ])("rejects %j as an invalid URL", (value) => {
    expect(() => resolveAccountsApiOrigin(value)).toThrow(
      "PUBLIC_API_ORIGIN is not a valid URL",
    );
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

describe("readAccountsConfig", () => {
  test("reads the API origin and leaves sign-in and checkout unconfigured by default", () => {
    expect(readAccountsConfig({})).toEqual({
      apiOrigin: DEFAULT_ACCOUNTS_API_ORIGIN,
      logto: undefined,
      paddle: undefined,
    });
  });

  test("requests the accounts:billing scope for the accounts-api resource", () => {
    const config = readAccountsConfig({
      PUBLIC_LOGTO_ENDPOINT: " https://auth.delino.io/ ",
      PUBLIC_LOGTO_APP_ID: " app-id ",
    });
    expect(config.logto).toEqual({
      endpoint: "https://auth.delino.io",
      appId: "app-id",
      scopes: [REQUIRED_API_SCOPE],
    });
    expect(REQUIRED_API_SCOPE).toBe("accounts:billing");
    expect(API_RESOURCE).toBe("https://accounts.api.delino.io");
  });

  test.each([
    [{ PUBLIC_LOGTO_ENDPOINT: "https://auth.delino.io" }, "set together"],
    [{ PUBLIC_LOGTO_APP_ID: "app" }, "set together"],
    [
      {
        PUBLIC_LOGTO_ENDPOINT: "https://auth.delino.io/oidc",
        PUBLIC_LOGTO_APP_ID: "a",
      },
      "PUBLIC_LOGTO_ENDPOINT must be an origin",
    ],
    [
      {
        PUBLIC_LOGTO_ENDPOINT: "https://u:p@auth.delino.io",
        PUBLIC_LOGTO_APP_ID: "a",
      },
      "PUBLIC_LOGTO_ENDPOINT must not contain credentials",
    ],
  ])("rejects an invalid Logto registration %#", (env, message) => {
    expect(() => readAccountsConfig(env)).toThrow(message);
  });

  test.each([
    ["https://auth.delino.io", "https://auth.delino.io"],
    ["https://auth.delino.io/", "https://auth.delino.io"],
    ["http://127.0.0.1:3001/", "http://127.0.0.1:3001"],
  ])("accepts the Logto origin %s", (endpoint, origin) => {
    expect(
      readAccountsConfig({
        PUBLIC_LOGTO_ENDPOINT: endpoint,
        PUBLIC_LOGTO_APP_ID: "a",
      }).logto?.endpoint,
    ).toBe(origin);
  });

  // The URL parser collapses these paths to `/` or treats a backslash as a
  // slash, so the raw text after the authority has to be rejected.
  test.each([
    "https://auth.delino.io/tenant/..",
    "https://auth.delino.io/..",
    "https://auth.delino.io/.",
    "https://auth.delino.io/%2e",
    "https://auth.delino.io/%2E%2e",
    "https://auth.delino.io/%2e%2e/",
    "https://auth.delino.io/tenant/../",
    "https://auth.delino.io\\",
    "https://auth.delino.io\\tenant",
    "https://auth.delino.io//",
    "https://auth.delino.io/tenant",
  ])("rejects the Logto endpoint path %s without echoing it", (endpoint) => {
    let message = "";
    try {
      readAccountsConfig({
        PUBLIC_LOGTO_ENDPOINT: endpoint,
        PUBLIC_LOGTO_APP_ID: "a",
      });
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toBe(
      "PUBLIC_LOGTO_ENDPOINT must be an origin without a path, query or fragment",
    );
    expect(message).not.toContain("auth.delino.io");
  });

  test("reads the Paddle checkout settings", () => {
    expect(
      readAccountsConfig({
        PUBLIC_PADDLE_CLIENT_TOKEN: "test_abc123",
        PUBLIC_PADDLE_ENVIRONMENT: "sandbox",
      }).paddle,
    ).toEqual({
      clientToken: "test_abc123",
      environment: PaddleEnvironment.Sandbox,
    });
    expect(
      readAccountsConfig({
        PUBLIC_PADDLE_CLIENT_TOKEN: "live_abc123",
        PUBLIC_PADDLE_ENVIRONMENT: "production",
      }).paddle,
    ).toEqual({
      clientToken: "live_abc123",
      environment: PaddleEnvironment.Production,
    });
    expect(
      readAccountsConfig({ PUBLIC_PADDLE_ENVIRONMENT: "sandbox" }).paddle,
    ).toBeUndefined();
  });

  test.each([
    [{ PUBLIC_PADDLE_ENVIRONMENT: "live" }, "must be sandbox or production"],
    [{ PUBLIC_PADDLE_CLIENT_TOKEN: "test_abc" }, "must be set with"],
    [
      {
        PUBLIC_PADDLE_CLIENT_TOKEN: "live_abc",
        PUBLIC_PADDLE_ENVIRONMENT: "sandbox",
      },
      "sandbox client-side token",
    ],
    [
      {
        PUBLIC_PADDLE_CLIENT_TOKEN: "test_abc",
        PUBLIC_PADDLE_ENVIRONMENT: "production",
      },
      "production client-side token",
    ],
    [
      {
        PUBLIC_PADDLE_CLIENT_TOKEN: "pdl_secret_key",
        PUBLIC_PADDLE_ENVIRONMENT: "sandbox",
      },
      "sandbox client-side token",
    ],
  ])("rejects invalid Paddle settings %#", (env, message) => {
    let error = "";
    try {
      readAccountsConfig(env);
    } catch (e) {
      error = (e as Error).message;
    }
    expect(error).toContain(message);
    // The message never echoes the token.
    expect(error).not.toContain("abc");
    expect(error).not.toContain("secret");
  });
});
