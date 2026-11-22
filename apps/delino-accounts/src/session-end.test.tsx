// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { QueryClient } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { LogtoAuthProvider } from "./auth.tsx";
import { Accounts } from "./App.tsx";
import { API_RESOURCE } from "./config.ts";
import { TEST_CONFIG, settle } from "./testing/render.tsx";

/**
 * Runs the real Logto SDK against a seeded session whose refresh token the
 * token endpoint rejects. The SDK keeps `isAuthenticated` true in that state,
 * so the page shows the ended session and its sign-in button, not the
 * signed-out screen.
 */
const SETTINGS = { endpoint: "https://logto.test", appId: "app", scopes: [] };

function base64Url(value: object): string {
  return btoa(JSON.stringify(value))
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replaceAll("=", "");
}

const realFetch = globalThis.fetch;
let requested: string[] = [];

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  requested = [];
  const key = (item: string) => `logto:${SETTINGS.appId}:${item}`;
  const idToken = [
    base64Url({ alg: "ES384", typ: "JWT" }),
    base64Url({ sub: "user-owner", iss: "https://logto.test/oidc" }),
    "signature",
  ].join(".");
  window.localStorage.setItem(key("idToken"), idToken);
  window.localStorage.setItem(key("refreshToken"), "expired-refresh-token");
  window.localStorage.setItem(
    key("accessToken"),
    JSON.stringify({
      [`@${API_RESOURCE}`]: { token: "old", scope: "", expiresAt: 1 },
    }),
  );
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(
      typeof input === "string" || input instanceof URL ? input : input.url,
    );
    requested.push(url.pathname);
    if (url.pathname === "/oidc/.well-known/openid-configuration") {
      return Response.json({
        authorization_endpoint: "https://logto.test/oidc/auth",
        token_endpoint: "https://logto.test/oidc/token",
        userinfo_endpoint: "https://logto.test/oidc/me",
        end_session_endpoint: "https://logto.test/oidc/session/end",
        revocation_endpoint: "https://logto.test/oidc/token/revocation",
        jwks_uri: "https://logto.test/oidc/jwks",
        issuer: "https://logto.test/oidc",
      });
    }
    if (url.pathname === "/oidc/token") {
      return Response.json({ error: "invalid_grant" }, { status: 400 });
    }
    return new Response("not found", { status: 404 });
  }) as typeof fetch;
});

afterEach(() => {
  cleanup();
  globalThis.fetch = realFetch;
});

describe("ended Logto session", () => {
  test("a rejected refresh token offers the sign-in redirect on a billing page", async () => {
    window.history.replaceState(null, "", "/billing");
    render(
      <LogtoAuthProvider settings={SETTINGS}>
        <Accounts
          config={TEST_CONFIG}
          checkout={undefined}
          queryClient={
            new QueryClient({ defaultOptions: { queries: { retry: false } } })
          }
        />
      </LogtoAuthProvider>,
    );
    expect(
      await screen.findByText("Your session ended. Sign in again."),
    ).toBeTruthy();
    expect(requested).toContain("/oidc/token");
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Sign in with Delino" }),
      );
    });
    await settle(100);
    // The SDK starts the redirect by saving its sign-in session.
    expect(
      window.sessionStorage.getItem(`logto:${SETTINGS.appId}:signInSession`),
    ).toBeTruthy();
  });
});
