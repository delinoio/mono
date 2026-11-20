// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  DEFAULT_API_ORIGIN,
  readConsoleConfig,
  resolveOrigin,
} from "./config.ts";

describe("readConsoleConfig", () => {
  test("uses the production API origin and no Logto settings by default", () => {
    expect(readConsoleConfig({})).toEqual({
      apiOrigin: DEFAULT_API_ORIGIN,
      logto: undefined,
      e2eFakeAuth: false,
    });
  });

  test("reads every variable", () => {
    expect(
      readConsoleConfig({
        PUBLIC_API_ORIGIN: "http://127.0.0.1:8300/",
        PUBLIC_LOGTO_ENDPOINT: "https://tenant.logto.app",
        PUBLIC_LOGTO_APP_ID: "abc_123-x",
        PUBLIC_OWNSAND_E2E_FAKE_AUTH: "1",
      }),
    ).toEqual({
      apiOrigin: "http://127.0.0.1:8300",
      logto: { endpoint: "https://tenant.logto.app", appId: "abc_123-x" },
      e2eFakeAuth: true,
    });
  });

  test("requires both Logto settings together", () => {
    expect(() =>
      readConsoleConfig({ PUBLIC_LOGTO_ENDPOINT: "https://tenant.logto.app" }),
    ).toThrow("must be set together");
    expect(() => readConsoleConfig({ PUBLIC_LOGTO_APP_ID: "abc" })).toThrow(
      "must be set together",
    );
  });

  test("rejects a Logto endpoint with a path and an invalid app ID", () => {
    expect(() =>
      readConsoleConfig({
        PUBLIC_LOGTO_ENDPOINT: "https://tenant.logto.app/oidc",
        PUBLIC_LOGTO_APP_ID: "abc",
      }),
    ).toThrow("PUBLIC_LOGTO_ENDPOINT must be an origin");
    expect(() =>
      readConsoleConfig({
        PUBLIC_LOGTO_ENDPOINT: "https://tenant.logto.app",
        PUBLIC_LOGTO_APP_ID: "a b",
      }),
    ).toThrow("PUBLIC_LOGTO_APP_ID must contain only");
  });

  test.each(["true", "0", "yes"])("rejects the e2e flag value %s", (value) => {
    expect(() =>
      readConsoleConfig({ PUBLIC_OWNSAND_E2E_FAKE_AUTH: value }),
    ).toThrow('must be unset or "1"');
  });
});

describe("resolveOrigin", () => {
  test("uses the fallback for an unset or ASCII-blank value", () => {
    expect(resolveOrigin("X", undefined, DEFAULT_API_ORIGIN)).toBe(
      DEFAULT_API_ORIGIN,
    );
    expect(resolveOrigin("X", " \t", DEFAULT_API_ORIGIN)).toBe(
      DEFAULT_API_ORIGIN,
    );
  });

  test("requires a value without a fallback", () => {
    expect(() => resolveOrigin("X", "", undefined)).toThrow("X is required");
  });

  test.each([
    "not a url",
    "ftp://ownsand.api.delino.io",
    "https://ownsand.api.delino.io/v1",
    "https://ownsand.api.delino.io/?debug=1",
    "https://ownsand.api.delino.io#",
    "https://ownsand.api.delino.io/..",
    " https://ownsand.api.delino.io",
  ])("rejects %j", (value) => {
    expect(() => resolveOrigin("PUBLIC_API_ORIGIN", value, undefined)).toThrow(
      "PUBLIC_API_ORIGIN",
    );
  });

  test.each([
    "https://user:pass@ownsand.api.delino.io",
    "token123:secret@host",
    "https://ownsand.api.delino.io/secret-path?token=pass",
  ])("does not echo the raw value of %j", (value) => {
    let message = "";
    try {
      resolveOrigin("PUBLIC_API_ORIGIN", value, undefined);
    } catch (error) {
      message = (error as Error).message;
    }
    expect(message).toContain("PUBLIC_API_ORIGIN");
    expect(message).not.toMatch(/pass|secret|token123|user/);
  });

  test("normalizes an explicit origin", () => {
    expect(
      resolveOrigin("X", "https://OWNSAND.api.delino.io/", undefined),
    ).toBe("https://ownsand.api.delino.io");
  });
});
