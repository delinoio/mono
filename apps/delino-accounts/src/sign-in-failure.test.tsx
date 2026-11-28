// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { QueryClient } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { Accounts } from "./App.tsx";
import { LogtoAuthProvider } from "./auth.tsx";
import { PENDING_INVITATION_KEY } from "./invitation.ts";
import { createFakeApi } from "./testing/fake-api.ts";
import { seedLogtoIdToken } from "./testing/logto-session.ts";
import { TEST_CONFIG } from "./testing/render.tsx";

/**
 * Runs the real Logto SDK against a tenant whose OIDC discovery request fails
 * or succeeds as the test says. `signIn` of the SDK
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
const CALLBACK_RECOVERY_TEXT =
  "The sign-in could not be completed. Sign in again.";

const APP_URL = "http://localhost:3000/";
const CALLBACK_URL = new URL("/callback", APP_URL).href;

const realFetch = globalThis.fetch;
let discoveryWorks = false;
let discoveryRequests = 0;
let discoveryGate: Promise<void> | undefined;

beforeEach(() => {
  // A started redirect navigates the window to the Logto tenant.
  (
    window as unknown as { happyDOM: { setURL(url: string): void } }
  ).happyDOM.setURL(APP_URL);
  window.sessionStorage.clear();
  window.localStorage.clear();
  discoveryWorks = false;
  discoveryRequests = 0;
  discoveryGate = undefined;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = new URL(
      typeof input === "string" || input instanceof URL ? input : input.url,
    );
    if (url.pathname === "/oidc/.well-known/openid-configuration") {
      discoveryRequests++;
      if (discoveryGate) {
        await discoveryGate;
      }
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

function renderAt(path: string, transport?: Transport) {
  window.history.replaceState(null, "", path);
  render(
    <LogtoAuthProvider settings={SETTINGS}>
      <Accounts
        config={TEST_CONFIG}
        transport={transport}
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
    "the billing sign-in view shows an alert, keeps the button, and a retry recovers",
    async () => {
      renderAt("/billing");
      const button = (await screen.findByRole(
        "button",
        { name: "Sign in with Delino" },
        { timeout: SDK_WAIT_MS },
      )) as HTMLButtonElement;
      expect(screen.queryByText(FAILURE_TEXT)).toBeNull();

      await click("Sign in with Delino");
      const alert = await screen.findByText(
        FAILURE_TEXT,
        {},
        { timeout: SDK_WAIT_MS },
      );
      expect(alert.getAttribute("role")).toBe("alert");
      // The raw SDK message never reaches the page.
      expect(document.body.textContent).not.toContain("secret-detail");
      expect(button.disabled).toBe(false);
      expect(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)).toBeNull();

      // Hold discovery open so the previous alert must clear before the retry fails.
      const beforeRetry = discoveryRequests;
      let releaseDiscovery!: () => void;
      discoveryGate = new Promise<void>((resolve) => {
        releaseDiscovery = resolve;
      });
      try {
        await click("Sign in with Delino");
        await waitFor(() => expect(discoveryRequests).toBe(beforeRetry + 1), {
          timeout: SDK_WAIT_MS,
        });
        expect(screen.queryByText(FAILURE_TEXT)).toBeNull();
      } finally {
        await act(async () => {
          discoveryGate = undefined;
          releaseDiscovery();
        });
      }

      const retryAlert = await screen.findByText(
        FAILURE_TEXT,
        {},
        { timeout: SDK_WAIT_MS },
      );
      expect(retryAlert.getAttribute("role")).toBe("alert");
      expect(
        screen.getByRole<HTMLButtonElement>("button", {
          name: "Sign in with Delino",
        }).disabled,
      ).toBe(false);

      // A successful retry clears the alert and starts the redirect to the
      // existing callback address.
      discoveryWorks = true;
      await click("Sign in with Delino");
      await expectRedirectStarted();
      await waitFor(() => expect(screen.queryByText(FAILURE_TEXT)).toBeNull(), {
        timeout: SDK_WAIT_MS,
      });
      expect(
        JSON.parse(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)!)
          .redirectUri,
      ).toBe(`${new URL(APP_URL).origin}/callback`);
      // The SDK navigated to the authorization endpoint of the tenant.
      expect(window.location.href).toContain("https://logto.test/oidc/auth");
    },
    TEST_TIMEOUT_MS,
  );

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

  test(
    "an ended invitation session shows a sign-in failure and clears it while retrying",
    async () => {
      const path = "/billing/invitations/accept";
      const invitationUrl = new URL(path, APP_URL).href;
      const signInAction = "Sign in with Delino";
      const api = createFakeApi();
      api.failNext.set(
        "AcceptInvitation",
        new ConnectError("token expired", Code.Unauthenticated),
      );
      const invitationRequests = () =>
        api.calls
          .filter(({ method }) => method === "AcceptInvitation")
          .map(({ request }) => request.invitationToken);

      seedLogtoIdToken(SETTINGS);
      window.sessionStorage.setItem(PENDING_INVITATION_KEY, TOKEN);
      renderAt(path, api.transport);

      await screen.findByRole(
        "button",
        { name: "Accept invitation" },
        { timeout: SDK_WAIT_MS },
      );
      await click("Accept invitation");
      await screen.findByText(
        "Your session ended. Sign in again.",
        {},
        { timeout: SDK_WAIT_MS },
      );
      screen.getByRole("button", { name: signInAction });
      expect(invitationRequests()).toEqual([TOKEN]);
      expect(screen.queryByText(FAILURE_TEXT)).toBeNull();

      await click(signInAction);
      const alert = await screen.findByText(
        FAILURE_TEXT,
        {},
        { timeout: SDK_WAIT_MS },
      );
      expect(alert.getAttribute("role")).toBe("alert");
      expect(document.body.textContent).not.toContain("secret-detail");
      const retryButton = screen.getByRole<HTMLButtonElement>("button", {
        name: signInAction,
      });
      expect(retryButton.disabled).toBe(false);
      expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe(TOKEN);
      expect(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)).toBeNull();
      expect(window.location.href).toBe(invitationUrl);

      const beforeRetry = discoveryRequests;
      let releaseDiscovery!: () => void;
      discoveryGate = new Promise<void>((resolve) => {
        releaseDiscovery = resolve;
      });
      discoveryWorks = true;
      try {
        await click(signInAction);
        await waitFor(() => expect(discoveryRequests).toBe(beforeRetry + 1), {
          timeout: SDK_WAIT_MS,
        });
        expect(screen.queryByText(FAILURE_TEXT)).toBeNull();
        expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe(
          TOKEN,
        );
        expect(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)).toBeNull();
        expect(window.location.href).toBe(invitationUrl);
        expect(invitationRequests()).toEqual([TOKEN]);
      } finally {
        await act(async () => {
          releaseDiscovery();
        });
      }

      await expectRedirectStarted();
      expect(
        JSON.parse(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)!)
          .redirectUri,
      ).toBe(new URL("/callback", APP_URL).href);
      const authorizationUrl = new URL(window.location.href);
      expect(authorizationUrl.origin + authorizationUrl.pathname).toBe(
        `${SETTINGS.endpoint}/oidc/auth`,
      );
      expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe(TOKEN);
      expect(invitationRequests()).toEqual([TOKEN]);
    },
    TEST_TIMEOUT_MS,
  );

  test.each([
    {
      recovery: "a failed callback",
      path: "/callback?error=access_denied&state=s",
      session: JSON.stringify({
        redirectUri: CALLBACK_URL,
        state: "s",
        codeVerifier: "v",
      }),
    },
    {
      recovery: "an unreadable session",
      path: "/callback",
      session: "{not json",
    },
  ])(
    "$recovery clears the sign-in alert while retry discovery is pending",
    async ({ path, session }) => {
      window.sessionStorage.setItem(SIGN_IN_SESSION_KEY, session);
      renderAt(path);

      const recoveryMessage = await screen.findByText(
        CALLBACK_RECOVERY_TEXT,
        {},
        { timeout: SDK_WAIT_MS },
      );
      const recoveryNotice = within(screen.getByRole("main")).getByRole(
        "alert",
      );
      expect(recoveryNotice.contains(recoveryMessage)).toBe(true);
      const signInButton = () =>
        within(recoveryNotice).getByRole<HTMLButtonElement>("button", {
          name: "Sign in with Delino",
        });
      expect(signInButton().disabled).toBe(false);
      expect(screen.queryByText(FAILURE_TEXT)).toBeNull();
      expect(discoveryRequests).toBe(0);

      await click("Sign in with Delino");
      const alert = await screen.findByText(
        FAILURE_TEXT,
        {},
        { timeout: SDK_WAIT_MS },
      );
      expect(alert.getAttribute("role")).toBe("alert");
      expect(alert.textContent).toBe(FAILURE_TEXT);
      expect(document.body.textContent).not.toContain("secret-detail");
      expect(signInButton().disabled).toBe(false);
      expect(discoveryRequests).toBe(1);
      expect(window.location.pathname).toBe("/callback");
      expect(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)).toBe(session);

      let releaseDiscovery!: () => void;
      discoveryGate = new Promise<void>((resolve) => {
        releaseDiscovery = resolve;
      });
      discoveryWorks = true;
      try {
        await click("Sign in with Delino");
        await waitFor(() => expect(discoveryRequests).toBe(2), {
          timeout: SDK_WAIT_MS,
        });
        expect(screen.queryByText(FAILURE_TEXT)).toBeNull();
        expect(screen.getByText(CALLBACK_RECOVERY_TEXT)).toBe(recoveryMessage);
        expect(window.location.pathname).toBe("/callback");
        expect(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)).toBe(
          session,
        );
      } finally {
        await act(async () => {
          releaseDiscovery();
        });
      }

      await waitFor(
        () =>
          expect(window.location.origin + window.location.pathname).toBe(
            "https://logto.test/oidc/auth",
          ),
        { timeout: SDK_WAIT_MS },
      );
      const replacement = window.sessionStorage.getItem(SIGN_IN_SESSION_KEY);
      expect(replacement).not.toBe(session);
      expect(JSON.parse(replacement!)).toMatchObject({
        redirectUri: CALLBACK_URL,
        state: expect.stringMatching(/.+/),
        codeVerifier: expect.stringMatching(/.+/),
      });
    },
    TEST_TIMEOUT_MS,
  );
});
