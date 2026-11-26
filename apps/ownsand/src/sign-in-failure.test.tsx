// SPDX-License-Identifier: Apache-2.0
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { App } from "./App.tsx";
import { API_RESOURCE, AuthMode } from "./config.ts";
import { pendingInvitation } from "./invitation.ts";

/**
 * Runs the real Logto SDK against a tenant whose OIDC discovery request
 * fails or succeeds as the test says. `signIn` of the SDK never rejects: it
 * stores the failure in its error state and resolves, so these tests check
 * what the page shows, not a rejected promise.
 */
const LOGTO_ENDPOINT = "https://logto.test";
const SIGN_IN_SESSION_KEY = "logto:app:signInSession";
const SECRET = "osi_TESTSECRET";
const FAILURE_TEXT =
  "Sign-in did not start. Check your connection and try again.";
const SDK_WAIT_MS = 20_000;
const TEST_TIMEOUT_MS = 2 * SDK_WAIT_MS + 5_000;

const CONFIG = {
  apiOrigin: API_RESOURCE,
  authMode: AuthMode.Logto,
  logto: { endpoint: LOGTO_ENDPOINT, appId: "app", scopes: ["ownsand:use"] },
};

const realFetch = globalThis.fetch;
let discoveryWorks = false;
let discoveryRequests = 0;
let tokenRequests = 0;

const APP_URL = "http://localhost:3000/";

beforeEach(() => {
  // A started redirect navigates the window to the Logto tenant.
  (
    window as unknown as { happyDOM: { setURL(url: string): void } }
  ).happyDOM.setURL(APP_URL);
  window.localStorage.clear();
  window.sessionStorage.clear();
  discoveryWorks = false;
  discoveryRequests = 0;
  tokenRequests = 0;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url === `${LOGTO_ENDPOINT}/oidc/.well-known/openid-configuration`) {
      discoveryRequests++;
      if (!discoveryWorks) {
        throw new TypeError("secret-detail: network down");
      }
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
      tokenRequests++;
      return Response.json(
        { code: "oidc.invalid_grant", message: "invalid_grant" },
        { status: 400 },
      );
    }
    throw new Error(`unexpected request ${url}`);
  }) as typeof fetch;
});

afterEach(() => {
  cleanup();
  globalThis.fetch = realFetch;
});

/** Stores a session whose access token expired, like session-ended.test.tsx. */
function storeSession() {
  const encode = (value: unknown) =>
    btoa(JSON.stringify(value))
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/, "");
  const now = Math.floor(Date.now() / 1000);
  window.localStorage.setItem(
    "logto:app:idToken",
    `${encode({ alg: "none", typ: "JWT" })}.${encode({
      sub: "user-1",
      iss: `${LOGTO_ENDPOINT}/oidc`,
      aud: "app",
      exp: now + 3600,
      iat: now,
    })}.`,
  );
  window.localStorage.setItem("logto:app:refreshToken", "refresh-token");
  window.localStorage.setItem(
    "logto:app:accessToken",
    JSON.stringify({
      [`@${API_RESOURCE}`]: {
        token: "expired",
        scope: "ownsand:use",
        expiresAt: now - 60,
      },
    }),
  );
}

function renderAt(path: string) {
  window.history.replaceState(null, "", path);
  render(<App config={CONFIG} />);
}

async function click(name: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
}

async function findButton(name: string) {
  return (await screen.findByRole(
    "button",
    { name },
    { timeout: SDK_WAIT_MS },
  )) as HTMLButtonElement;
}

/** Waits until one more discovery request was made, so a failed attempt ended. */
async function attemptEnded(before: number) {
  await waitFor(() => expect(discoveryRequests).toBeGreaterThan(before), {
    timeout: SDK_WAIT_MS,
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

async function expectAlertGone() {
  await waitFor(() => expect(screen.queryByText(FAILURE_TEXT)).toBeNull(), {
    timeout: SDK_WAIT_MS,
  });
}

describe("sign-in that fails before the redirect (real SDK)", () => {
  test(
    "the signed-out screen shows an alert, keeps the button, clears it on a retry and starts the redirect",
    async () => {
      renderAt("/");
      const button = await findButton("Sign in with Delino");
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

      // A repeated failure shows the alert again.
      const before = discoveryRequests;
      await click("Sign in with Delino");
      await attemptEnded(before);
      expect(await screen.findByText(FAILURE_TEXT)).toBeTruthy();

      // A successful retry clears the alert and starts the redirect.
      discoveryWorks = true;
      await click("Sign in with Delino");
      await expectRedirectStarted();
      await expectAlertGone();
      expect(
        JSON.parse(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)!)
          .redirectUri,
      ).toBe(`${new URL(APP_URL).origin}/callback`);
      // The SDK navigated to the authorization endpoint of the tenant.
      expect(window.location.href).toContain(`${LOGTO_ENDPOINT}/oidc/auth`);
    },
    TEST_TIMEOUT_MS,
  );

  test(
    "Sign in to accept shows the alert, keeps the secret and recovers",
    async () => {
      renderAt(`/invitations/accept#secret=${SECRET}`);
      await findButton("Sign in to accept");
      await click("Sign in to accept");
      expect(
        (
          await screen.findByText(FAILURE_TEXT, {}, { timeout: SDK_WAIT_MS })
        ).getAttribute("role"),
      ).toBe("alert");
      expect(pendingInvitation.read()).toBe(SECRET);
      expect(
        (
          screen.getByRole("button", {
            name: "Sign in to accept",
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false);

      discoveryWorks = true;
      await click("Sign in to accept");
      await expectRedirectStarted();
      await expectAlertGone();
      expect(pendingInvitation.read()).toBe(SECRET);
    },
    TEST_TIMEOUT_MS,
  );

  test(
    "sign-in from the ended-session notice shows the alert and recovers on retry",
    async () => {
      storeSession();
      // Allow discovery so the SDK reaches the token endpoint and receives
      // the expired-session error.
      discoveryWorks = true;
      renderAt("/");
      await screen.findByText(
        "Your session ended. Sign in again.",
        {},
        { timeout: SDK_WAIT_MS },
      );
      expect(tokenRequests).toBeGreaterThan(0);
      expect(screen.queryByText(FAILURE_TEXT)).toBeNull();

      // The token request cached discovery. Fail the next PKCE digest to stop
      // sign-in before the SDK saves a session or navigates.
      const digestError = new Error("test PKCE digest failure");
      const digest = rs
        .spyOn(globalThis.crypto.subtle, "digest")
        .mockRejectedValueOnce(digestError);
      try {
        await click("Sign in with Delino");
        const alert = await screen.findByText(
          FAILURE_TEXT,
          {},
          { timeout: SDK_WAIT_MS },
        );
        expect(alert.getAttribute("role")).toBe("alert");
        expect(digest).toHaveBeenCalledTimes(1);
        expect(document.body.textContent).not.toContain(digestError.message);
        expect(
          (
            screen.getByRole("button", {
              name: "Sign in with Delino",
            }) as HTMLButtonElement
          ).disabled,
        ).toBe(false);
        expect(window.location.href).toBe(APP_URL);
        expect(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)).toBeNull();
      } finally {
        digest.mockRestore();
      }

      await click("Sign in with Delino");
      await expectRedirectStarted();
      expect(screen.queryByText(FAILURE_TEXT)).toBeNull();
      expect(
        JSON.parse(window.sessionStorage.getItem(SIGN_IN_SESSION_KEY)!)
          .redirectUri,
      ).toBe(`${new URL(APP_URL).origin}/callback`);
      const authorizationUrl = new URL(window.location.href);
      expect(authorizationUrl.origin + authorizationUrl.pathname).toBe(
        `${LOGTO_ENDPOINT}/oidc/auth`,
      );
    },
    TEST_TIMEOUT_MS + SDK_WAIT_MS,
  );
});
