// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { AuthMode, readConsoleConfig } from "./config.ts";

const MALFORMED_ENDPOINTS = ["not-a-url", "https://login.example/path"];
const APP_IDS = [undefined, "", "   ", "app"];

describe("Logto endpoint validation", () => {
  test.each(
    MALFORMED_ENDPOINTS.flatMap((endpoint) =>
      APP_IDS.flatMap((appId) =>
        [true, false].map((production) => ({ endpoint, appId, production })),
      ),
    ),
  )(
    "rejects the endpoint $endpoint with the app ID $appId (production: $production)",
    ({ endpoint, appId, production }) => {
      expect(() =>
        readConsoleConfig(
          { PUBLIC_LOGTO_ENDPOINT: endpoint, PUBLIC_LOGTO_APP_ID: appId },
          { production },
        ),
      ).toThrow("PUBLIC_LOGTO_ENDPOINT");
    },
  );

  test.each([true, false])(
    "an incomplete valid registration has no Logto settings (production: %s)",
    (production) => {
      const endpointOnly = readConsoleConfig(
        { PUBLIC_LOGTO_ENDPOINT: "https://login.example" },
        { production },
      );
      const blankAppId = readConsoleConfig(
        {
          PUBLIC_LOGTO_ENDPOINT: "https://login.example",
          PUBLIC_LOGTO_APP_ID: "  ",
        },
        { production },
      );
      const appIdOnly = readConsoleConfig(
        { PUBLIC_LOGTO_APP_ID: "app" },
        { production },
      );
      const neither = readConsoleConfig({}, { production });
      for (const config of [endpointOnly, blankAppId, appIdOnly, neither]) {
        expect(config.authMode).toBe(AuthMode.Logto);
        expect(config.logto).toBeUndefined();
      }
    },
  );

  test("a complete registration gives the normalized origin and scopes", () => {
    expect(
      readConsoleConfig(
        {
          PUBLIC_LOGTO_ENDPOINT: " https://login.example/ ",
          PUBLIC_LOGTO_APP_ID: " app ",
          PUBLIC_LOGTO_SCOPES: "read:sandboxes ownsand:use",
        },
        { production: true },
      ).logto,
    ).toEqual({
      endpoint: "https://login.example",
      appId: "app",
      scopes: ["ownsand:use", "read:sandboxes"],
    });
  });

  test("the mock API keeps priority over a malformed endpoint", () => {
    const env = {
      PUBLIC_OWNSAND_MOCK_API: "true",
      PUBLIC_LOGTO_ENDPOINT: "not-a-url",
    };
    const config = readConsoleConfig(env, { production: false });
    expect(config.authMode).toBe(AuthMode.Mock);
    expect(config.logto).toBeUndefined();
    expect(() => readConsoleConfig(env, { production: true })).toThrow(
      "PUBLIC_OWNSAND_MOCK_API must not be set for a production build",
    );
  });
});
