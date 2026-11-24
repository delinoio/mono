// SPDX-License-Identifier: Apache-2.0
import type { Browser } from "@e2e-dev/web";
import { API_RESOURCE } from "../src/config.ts";

export const API_ORIGIN = "http://127.0.0.1:28100";
const LOGTO_ENDPOINT = "https://logto.test";
const LOGTO_APP_ID = "accounts-e2e";

export async function seedFixtureSession(browser: Browser, appOrigin: string) {
  // Let the real SDK read a fixture session. Its tokens are only usable with
  // the browser's intercepted responses.
  await browser.addInitScript(
    ({ appId, appOrigin, endpoint, resource }) => {
      if (location.origin !== appOrigin) return;
      const prefix = `logto:${appId}`;
      const header = btoa(JSON.stringify({ alg: "ES384", typ: "JWT" }));
      const now = Math.floor(Date.now() / 1000);
      const claims = btoa(
        JSON.stringify({
          sub: "fixture-user",
          iss: `${endpoint}/oidc`,
          aud: appId,
          iat: now,
          exp: now + 3600,
        }),
      );
      localStorage.setItem(`${prefix}:idToken`, `${header}.${claims}.fixture`);
      localStorage.setItem(
        `${prefix}:accessToken`,
        JSON.stringify({
          [`@${resource}`]: {
            token: "fixture-token",
            scope: "accounts:billing",
            expiresAt: now + 3600,
          },
        }),
      );
    },
    {
      appId: LOGTO_APP_ID,
      appOrigin,
      endpoint: LOGTO_ENDPOINT,
      resource: API_RESOURCE,
    },
  );
}
