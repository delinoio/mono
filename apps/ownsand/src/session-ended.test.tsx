// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { cleanup, render, screen } from "@testing-library/react";
import { App } from "./App.tsx";
import { API_RESOURCE, AuthMode } from "./config.ts";

const LOGTO_ENDPOINT = "https://logto.test";

/** An unsigned JWT: the SDK reads the claims of the stored ID token only. */
function unsignedJwt(claims: Record<string, unknown>): string {
  const encode = (value: unknown) =>
    btoa(JSON.stringify(value))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
  return `${encode({ alg: "none", typ: "JWT" })}.${encode(claims)}.`;
}

const originalFetch = globalThis.fetch;

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/");
});

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
});

describe("ended Logto session (real SDK)", () => {
  test("offers sign-in when the refresh token request is rejected", async () => {
    const fetched: string[] = [];
    globalThis.fetch = (async (
      input: RequestInfo | URL,
      init?: RequestInit,
    ) => {
      const url = input instanceof Request ? input.url : String(input);
      fetched.push(`${init?.method ?? "GET"} ${url}`);
      if (url === `${LOGTO_ENDPOINT}/oidc/.well-known/openid-configuration`) {
        return Response.json({
          authorization_endpoint: `${LOGTO_ENDPOINT}/oidc/auth`,
          token_endpoint: `${LOGTO_ENDPOINT}/oidc/token`,
          end_session_endpoint: `${LOGTO_ENDPOINT}/oidc/session/end`,
          revocation_endpoint: `${LOGTO_ENDPOINT}/oidc/token/revocation`,
          jwks_uri: `${LOGTO_ENDPOINT}/oidc/jwks`,
          issuer: `${LOGTO_ENDPOINT}/oidc`,
          userinfo_endpoint: `${LOGTO_ENDPOINT}/oidc/me`,
        });
      }
      if (url === `${LOGTO_ENDPOINT}/oidc/token`) {
        return Response.json(
          { code: "oidc.invalid_grant", message: "invalid_grant" },
          { status: 400 },
        );
      }
      throw new Error(`unexpected request ${url}`);
    }) as typeof fetch;

    const now = Math.floor(Date.now() / 1000);
    window.localStorage.setItem(
      "logto:app:idToken",
      unsignedJwt({
        sub: "user-1",
        iss: `${LOGTO_ENDPOINT}/oidc`,
        aud: "app",
        exp: now + 3600,
        iat: now,
      }),
    );
    window.localStorage.setItem("logto:app:refreshToken", "refresh-token");
    window.localStorage.setItem(
      "logto:app:accessToken",
      JSON.stringify({
        // The SDK key is "<sorted scopes>@<resource>" and has no scope here.
        [`@${API_RESOURCE}`]: {
          token: "expired",
          scope: "ownsand:use",
          expiresAt: now - 60,
        },
      }),
    );

    render(
      <App
        config={{
          apiOrigin: API_RESOURCE,
          authMode: AuthMode.Logto,
          logto: {
            endpoint: LOGTO_ENDPOINT,
            appId: "app",
            scopes: ["ownsand:use"],
          },
        }}
      />,
    );
    expect(
      await screen.findByText("Your session ended. Sign in again."),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Sign in with Delino" }),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    // The refresh failed, so the API was never called.
    expect(fetched.some((entry) => entry.includes(API_RESOURCE))).toBe(false);
  });
});
