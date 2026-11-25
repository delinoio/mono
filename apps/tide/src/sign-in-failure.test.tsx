// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { App } from "./App.tsx";
import { API_RESOURCE, AuthMode, REQUIRED_API_SCOPE } from "./config.ts";

const APP_URL = "http://localhost:3000";
const LOGTO_ENDPOINT = "https://logto.test";
const SESSION_KEY = "logto:app:signInSession";
const FAILURE_TEXT =
  "Sign-in did not start. Check your connection and try again.";
const SESSION_ENDED_TEXT = "Your session ended. Sign in again.";
const DIAGNOSTIC = "synthetic discovery diagnostic";
const SDK_WAIT = { timeout: 10_000 };
const ORGANIZATION_ID = "0193b000-0000-7000-8000-000000000001";
const ORGANIZATION_RPC = `${API_RESOURCE}/delino.tide.v1.OrganizationService`;

const config = {
  apiOrigin: API_RESOURCE,
  authMode: AuthMode.Logto,
  logto: {
    endpoint: LOGTO_ENDPOINT,
    appId: "app",
    scopes: [REQUIRED_API_SCOPE],
  },
};

const realFetch = globalThis.fetch;
let requestDiscovery: () => Promise<Response>;
let protectedRequests: string[];

beforeEach(() => {
  (
    window as unknown as { happyDOM: { setURL(url: string): void } }
  ).happyDOM.setURL(APP_URL);
  window.localStorage.clear();
  window.sessionStorage.clear();
  protectedRequests = [];
  requestDiscovery = async () => {
    throw new TypeError(DIAGNOSTIC);
  };
  globalThis.fetch = async (input) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url === `${LOGTO_ENDPOINT}/oidc/.well-known/openid-configuration`) {
      return requestDiscovery();
    }
    if (url.startsWith(`${ORGANIZATION_RPC}/`)) {
      protectedRequests.push(url);
      return Response.json({ code: "unauthenticated" }, { status: 401 });
    }
    throw new Error("Unexpected request from the SDK");
  };
});

afterEach(() => {
  cleanup();
  globalThis.fetch = realFetch;
});

function renderAt(path: string) {
  window.history.replaceState(null, "", path);
  render(<App config={config} />);
}

function findSignIn() {
  return screen.findByRole<HTMLButtonElement>(
    "button",
    { name: "Sign in with Delino" },
    SDK_WAIT,
  );
}

function signInScreen() {
  return within(screen.getByRole("main"));
}

function restoreLogtoSession() {
  const now = Math.floor(Date.now() / 1000);
  const claims = {
    iss: `${LOGTO_ENDPOINT}/oidc`,
    sub: "fixture-user",
    aud: config.logto.appId,
    iat: now,
    exp: now + 3600,
  };
  window.localStorage.setItem(
    "logto:app:idToken",
    `header.${Buffer.from(JSON.stringify(claims)).toString("base64url")}.signature`,
  );
  window.localStorage.setItem(
    "logto:app:accessToken",
    JSON.stringify({
      [`@${API_RESOURCE}`]: {
        token: "fixture-access-token",
        scope: REQUIRED_API_SCOPE,
        expiresAt: claims.exp,
      },
    }),
  );
}

describe("Logto sign-in before the redirect", () => {
  test.each([
    { path: "/", method: undefined },
    { path: "/", method: "ListOrganizations" },
    { path: `/orgs/${ORGANIZATION_ID}`, method: "GetOrganization" },
  ])(
    "handles failure and retry on $path (RPC: $method)",
    async ({ path, method }) => {
      if (method !== undefined) restoreLogtoSession();
      renderAt(path);
      const signIn = await findSignIn();
      const main = signInScreen();
      const alert = { selector: '[role="alert"]' };
      expect(main.queryByText(FAILURE_TEXT)).toBeNull();
      if (method !== undefined) {
        expect(protectedRequests).toContain(`${ORGANIZATION_RPC}/${method}`);
        expect(main.getByText(SESSION_ENDED_TEXT)).toBeTruthy();
      }

      fireEvent.click(signIn);
      const failure = await main.findByText(FAILURE_TEXT, alert, SDK_WAIT);
      expect(failure.parentElement?.closest('[role="alert"]')).toBeNull();
      if (method !== undefined) {
        expect(main.getByText(SESSION_ENDED_TEXT, alert)).toBeTruthy();
      }
      expect(document.body.textContent).not.toContain(DIAGNOSTIC);
      expect(signIn.disabled).toBe(false);
      expect(window.location.origin).toBe(APP_URL);
      expect(window.location.pathname).toBe(path);
      expect(window.sessionStorage.getItem(SESSION_KEY)).toBeNull();

      let finishRetry!: () => void;
      const retry = new Promise<void>((resolve) => {
        finishRetry = resolve;
      });
      requestDiscovery = async () => {
        await retry;
        throw new TypeError(DIAGNOSTIC);
      };
      fireEvent.click(signIn);
      expect(main.queryByText(FAILURE_TEXT)).toBeNull();
      await act(async () => {
        finishRetry();
      });
      expect(await main.findByText(FAILURE_TEXT, alert, SDK_WAIT)).toBeTruthy();
      expect(signIn.disabled).toBe(false);

      requestDiscovery = async () =>
        Response.json({
          authorization_endpoint: `${LOGTO_ENDPOINT}/oidc/auth`,
          token_endpoint: `${LOGTO_ENDPOINT}/oidc/token`,
          end_session_endpoint: `${LOGTO_ENDPOINT}/oidc/session/end`,
          revocation_endpoint: `${LOGTO_ENDPOINT}/oidc/token/revocation`,
          jwks_uri: `${LOGTO_ENDPOINT}/oidc/jwks`,
          issuer: `${LOGTO_ENDPOINT}/oidc`,
          userinfo_endpoint: `${LOGTO_ENDPOINT}/oidc/me`,
        });
      fireEvent.click(signIn);
      await waitFor(
        () => expect(window.location.origin).toBe(LOGTO_ENDPOINT),
        SDK_WAIT,
      );
      expect(window.location.pathname).toBe("/oidc/auth");
      expect(
        new URLSearchParams(window.location.search).get("redirect_uri"),
      ).toBe(`${APP_URL}/callback`);
      expect(main.queryByText(FAILURE_TEXT)).toBeNull();
    },
    30_000,
  );

  test.each([
    [
      "/invitations/accept#secret=fixture",
      "Sign in, then open the invitation link again.",
    ],
    [
      "/github/setup",
      "Sign in, then start the GitHub installation again from the integrations of your organization.",
    ],
  ])(
    "keeps the signed-out guidance on %s",
    async (path, notice) => {
      renderAt(path);
      fireEvent.click(await findSignIn());

      expect(
        (await signInScreen().findByRole("alert", {}, SDK_WAIT)).textContent,
      ).toBe(FAILURE_TEXT);
      expect(screen.getByText(notice)).toBeTruthy();
    },
    15_000,
  );

  test("shows the registration notice when the build has no Logto settings", () => {
    render(<App config={{ ...config, logto: undefined }} />);

    expect(signInScreen().getByRole("alert").textContent).toBe(
      "Sign-in is not configured for this build.",
    );
    expect(
      screen.queryByRole("button", { name: "Sign in with Delino" }),
    ).toBeNull();
  });
});
