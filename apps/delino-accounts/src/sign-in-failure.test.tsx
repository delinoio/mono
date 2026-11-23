// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { QueryClient } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { Accounts } from "./App.tsx";
import { LogtoAuthProvider } from "./auth.tsx";
import { PENDING_INVITATION_KEY } from "./invitation.ts";
import { TEST_CONFIG } from "./testing/render.tsx";

/**
 * Runs the real Logto SDK without a session against a tenant whose OIDC
 * discovery request fails or succeeds as the test says. `signIn` of the SDK
 * never rejects: it stores the failure in its error state and resolves, so
 * these tests check what the page shows, not a rejected promise.
 */
const SETTINGS = { endpoint: "https://logto.test", appId: "app", scopes: [] };
const SIGN_IN_SESSION_KEY = `logto:${SETTINGS.appId}:signInSession`;
const SDK_WAIT_MS = 20_000;
const TEST_TIMEOUT_MS = 2 * SDK_WAIT_MS + 5_000;
const TOKEN = "bai_TESTTOKEN";
const FAILURE_TEXT =
  "Sign-in did not start. Check your connection and try again.";

const realFetch = globalThis.fetch;
let discoveryWorks = false;
let discoveryRequests = 0;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  discoveryWorks = false;
  discoveryRequests = 0;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(
      typeof input === "string" || input instanceof URL ? input : input.url,
    );
    if (url.pathname === "/oidc/.well-known/openid-configuration") {
      discoveryRequests++;
      if (!discoveryWorks) {
        throw new TypeError("secret-detail: network down");
      }
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
    return new Response("not found", { status: 404 });
  }) as typeof fetch;
});

afterEach(() => {
  cleanup();
  globalThis.fetch = realFetch;
});

function renderAt(path: string) {
  window.history.replaceState(null, "", path);
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
}

async function click(name: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
}

/** Waits for the redirect start: the SDK saved its sign-in session. */
async function expectRedirectStarted() {
  await waitFor(
    () =>
      expect(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)).toBeTruthy(),
    { timeout: SDK_WAIT_MS },
  );
}

describe("sign-in that fails before the redirect", () => {
  test(
    "Sign in to accept shows the alert, keeps the pending invitation and recovers",
    async () => {
      renderAt(`/billing/invitations/accept#token=${TOKEN}`);
      await screen.findByRole(
        "button",
        { name: "Sign in to accept" },
        { timeout: SDK_WAIT_MS },
      );
      await click("Sign in to accept");
      expect(
        (
          await screen.findByText(FAILURE_TEXT, {}, { timeout: SDK_WAIT_MS })
        ).getAttribute("role"),
      ).toBe("alert");
      expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe(TOKEN);
      const button = screen.getByRole("button", {
        name: "Sign in to accept",
      }) as HTMLButtonElement;
      expect(button.disabled).toBe(false);

      discoveryWorks = true;
      await click("Sign in to accept");
      await expectRedirectStarted();
      expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe(TOKEN);
      await waitFor(() => expect(screen.queryByText(FAILURE_TEXT)).toBeNull(), {
        timeout: SDK_WAIT_MS,
      });
    },
    TEST_TIMEOUT_MS,
  );
});
