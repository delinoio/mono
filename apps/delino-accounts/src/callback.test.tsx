// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider } from "@logto/react";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";
import { StrictMode } from "react";
import { Accounts } from "./App.tsx";
import {
  type Auth,
  AuthContextProvider,
  isLogtoSignInPending,
} from "./auth.tsx";
import { pendingInvitation } from "./invitation.ts";
import { TEST_CONFIG } from "./testing/render.tsx";

const APP_ID = "app";
const SIGN_IN_SESSION_KEY = `logto:${APP_ID}:signInSession`;
const TOKEN = "bai_TESTTOKEN";

/** Stores the sign-in session that the Logto SDK keeps while a redirect is out. */
function storeSignInSession(
  redirectUri = `${window.location.origin}/callback`,
) {
  window.sessionStorage.setItem(
    SIGN_IN_SESSION_KEY,
    JSON.stringify({ redirectUri, codeVerifier: "v", state: "s" }),
  );
}

/** What the fake client's `handleSignInCallback` does in the current test. */
let handleSignInCallback: () => Promise<void> = async () => {};
/** How often the SDK client completed a sign-in redirect. */
let completions = 0;

/**
 * A Logto client with the redirect rule of `@logto/client`: a redirect is
 * pending only while the stored sign-in session names this address. A real
 * redirect round trip needs a Logto tenant, so the tests replace the network
 * part and keep the SDK provider, hook and the app real.
 */
class FakeLogtoClient {
  isSignInRedirected = async (url: string) => {
    const stored = window.sessionStorage.getItem(SIGN_IN_SESSION_KEY);
    if (stored === null) {
      return false;
    }
    const { origin, pathname } = new URL(url);
    return JSON.parse(stored).redirectUri === `${origin}${pathname}`;
  };
  handleSignInCallback = async () => {
    completions++;
    await handleSignInCallback();
    window.sessionStorage.removeItem(SIGN_IN_SESSION_KEY);
  };
  isAuthenticated = async () => false;
}

const AUTH: Auth = {
  isLoading: false,
  isAuthenticated: false,
  userId: undefined,
  signIn: async () => {},
  signOut: async () => {},
  getAccessToken: async () => undefined,
};

function renderCallback(
  options: { strict?: boolean; url?: string } = {},
): void {
  window.history.replaceState(null, "", options.url ?? "/callback");
  const app = (
    <LogtoProvider
      config={{ endpoint: "https://logto.test", appId: APP_ID }}
      LogtoClientClass={FakeLogtoClient as never}
    >
      <AuthContextProvider value={AUTH}>
        <Accounts
          config={TEST_CONFIG}
          queryClient={new QueryClient()}
          checkout={undefined}
        />
      </AuthContextProvider>
    </LogtoProvider>
  );
  render(options.strict ? <StrictMode>{app}</StrictMode> : app);
}

async function settle(ms = 50) {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  handleSignInCallback = async () => {};
  completions = 0;
});

afterEach(() => {
  cleanup();
});

describe("sign-in callback without a redirect in progress", () => {
  test.each([false, true])(
    "leaves for billing (StrictMode %s)",
    async (strict) => {
      renderCallback({ strict });
      await settle();
      expect(window.location.pathname).toBe("/billing");
      expect(completions).toBe(0);
    },
  );

  test("replaces the history entry instead of adding one", async () => {
    const before = window.history.length;
    renderCallback();
    await settle();
    expect(window.history.length).toBe(before);
  });

  test("leaves for the pending invitation and keeps its token", async () => {
    pendingInvitation.save(TOKEN);
    renderCallback();
    await settle();
    expect(window.location.pathname).toBe("/billing/invitations/accept");
    expect(pendingInvitation.read()).toBe(TOKEN);
  });

  test("ignores a code and a state without a stored sign-in session", async () => {
    renderCallback({ url: "/callback?code=abc&state=xyz" });
    await settle();
    expect(window.location.pathname).toBe("/billing");
    expect(completions).toBe(0);
  });
});

describe("sign-in callback with a redirect in progress", () => {
  test("navigates only after the sign-in callback resolves, and once", async () => {
    storeSignInSession();
    let release: () => void = () => {};
    handleSignInCallback = () =>
      new Promise<void>((resolve) => {
        release = resolve;
      });
    renderCallback({ url: "/callback?code=abc&state=xyz" });
    await settle();
    expect(window.location.pathname).toBe("/callback");
    expect(completions).toBe(1);
    await act(async () => {
      release();
    });
    await settle();
    expect(window.location.pathname).toBe("/billing");
    expect(completions).toBe(1);
  });

  test("completes a redirect once under StrictMode and keeps the token", async () => {
    storeSignInSession();
    pendingInvitation.save(TOKEN);
    renderCallback({ strict: true });
    await settle();
    expect(window.location.pathname).toBe("/billing/invitations/accept");
    expect(pendingInvitation.read()).toBe(TOKEN);
  });

  test.each([false, true])(
    "a failed callback shows the notice, stays and deletes the token (StrictMode %s)",
    async (strict) => {
      storeSignInSession();
      pendingInvitation.save(TOKEN);
      handleSignInCallback = async () => {
        throw new Error("callback failed");
      };
      renderCallback({ strict });
      await settle();
      expect(screen.getByText("Something went wrong. Try again.")).toBeTruthy();
      expect(window.location.pathname).toBe("/callback");
      expect(pendingInvitation.read()).toBeUndefined();
    },
  );

  test("a callback in progress keeps the stored token", async () => {
    storeSignInSession();
    pendingInvitation.save(TOKEN);
    handleSignInCallback = () => new Promise<void>(() => {});
    renderCallback();
    await settle();
    expect(window.location.pathname).toBe("/callback");
    expect(pendingInvitation.read()).toBe(TOKEN);
  });
});

describe("isLogtoSignInPending", () => {
  const location = { origin: "https://accounts.test", pathname: "/callback" };
  const session = (redirectUri: string) =>
    JSON.stringify({ redirectUri, codeVerifier: "v", state: "s" });
  const storage = (items: Record<string, string>) => ({
    getItem: (key: string) => items[key] ?? null,
  });

  test("is false without a stored session", () => {
    expect(isLogtoSignInPending("app", location, storage({}))).toBe(false);
    expect(isLogtoSignInPending("app", location, undefined)).toBe(false);
  });

  test("is true when the session redirect URI is this address", () => {
    const items = {
      "logto:app:signInSession": session("https://accounts.test/callback"),
    };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(true);
  });

  test("reads the legacy session key", () => {
    const items = { "logto:app": session("https://accounts.test/callback") };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(true);
  });

  test("ignores the session of another application", () => {
    const items = {
      "logto:other:signInSession": session("https://accounts.test/callback"),
    };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(false);
  });

  test("is false when the session belongs to another address", () => {
    const items = {
      "logto:app:signInSession": session("https://accounts.test/other"),
    };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(false);
  });

  test("is true for a session that is not readable, so the hook reports it", () => {
    for (const value of ["{not json", "{}"]) {
      expect(
        isLogtoSignInPending(
          "app",
          location,
          storage({ "logto:app:signInSession": value }),
        ),
      ).toBe(true);
    }
  });

  test("is false when the storage throws", () => {
    const throwing = {
      getItem: () => {
        throw new Error("blocked");
      },
    };
    expect(isLogtoSignInPending("app", location, throwing)).toBe(false);
  });
});
