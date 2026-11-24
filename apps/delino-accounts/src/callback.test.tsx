// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider } from "@logto/react";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { StrictMode } from "react";
import { Accounts } from "./App.tsx";
import {
  type Auth,
  AuthContextProvider,
  LogtoSignInRedirect,
  readLogtoSignInRedirect,
} from "./auth.tsx";
import { pendingInvitation } from "./invitation.ts";
import { TEST_CONFIG } from "./testing/render.tsx";

const APP_ID = "app";
const SIGN_IN_SESSION_KEY = `logto:${APP_ID}:signInSession`;
const TOKEN = "bai_TESTTOKEN";

const SIGN_IN_FAILURE_MESSAGE =
  "Sign-in did not start. Check your connection and try again.";
const RECOVERY_MESSAGE = "The sign-in could not be completed. Sign in again.";

/** Finds the callback recovery notice among the app alerts. */
function recoveryNotice(): HTMLElement {
  const notice = screen.getByText(RECOVERY_MESSAGE).closest("[role=alert]");
  if (!(notice instanceof HTMLElement)) {
    throw new Error("the failure message is not inside an alert");
  }
  return notice;
}

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
/** How often the sign-in button started a redirect. */
let signIns = 0;
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
    // Like the SDK, throw for a session that is not readable.
    const session = JSON.parse(stored);
    if (
      typeof session?.redirectUri !== "string" ||
      typeof session.codeVerifier !== "string" ||
      typeof session.state !== "string"
    ) {
      throw new Error("invalid sign-in session");
    }
    return session.redirectUri === `${origin}${pathname}`;
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
  signIn: async () => {
    signIns++;
  },
  signInFailed: false,
  signOut: async () => {},
  getAccessToken: async () => undefined,
};

function renderCallback(
  options: { strict?: boolean; url?: string; auth?: Partial<Auth> } = {},
): void {
  window.history.replaceState(null, "", options.url ?? "/callback");
  const app = (
    <LogtoProvider
      config={{ endpoint: "https://logto.test", appId: APP_ID }}
      LogtoClientClass={FakeLogtoClient as never}
    >
      <AuthContextProvider value={{ ...AUTH, ...options.auth }}>
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
  signIns = 0;
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
    "a failed callback offers sign-in again, stays and deletes the token (StrictMode %s)",
    async (strict) => {
      storeSignInSession();
      pendingInvitation.save(TOKEN);
      handleSignInCallback = async () => {
        throw new Error("callback failed");
      };
      renderCallback({ strict, url: "/callback?error=access_denied&state=s" });
      await settle();

      const button = within(recoveryNotice()).getByRole<HTMLButtonElement>(
        "button",
        { name: "Sign in with Delino" },
      );
      expect(button.disabled).toBe(false);
      expect(screen.queryByText("callback failed")).toBeNull();
      expect(screen.queryByText(SIGN_IN_FAILURE_MESSAGE)).toBeNull();
      expect(window.location.pathname).toBe("/callback");
      expect(pendingInvitation.read()).toBeUndefined();

      await act(async () => {
        button.click();
      });
      expect(signIns).toBe(1);
    },
  );

  test("a retry that fails before redirect shows the alert and can be retried", async () => {
    storeSignInSession();
    handleSignInCallback = async () => {
      throw new Error("callback failed");
    };
    renderCallback({ auth: { signInFailed: true } });
    await settle();

    expect(screen.getByText(SIGN_IN_FAILURE_MESSAGE)).toBeTruthy();
    const button = within(recoveryNotice()).getByRole<HTMLButtonElement>(
      "button",
      { name: "Sign in with Delino" },
    );
    expect(button.disabled).toBe(false);
    await act(async () => {
      button.click();
    });
    expect(signIns).toBe(1);
  });

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

describe("sign-in callback with an unreadable stored session", () => {
  let rejections: unknown[] = [];
  const onRejection = (event: PromiseRejectionEvent) => {
    rejections.push(event.reason);
  };
  beforeEach(() => {
    rejections = [];
    window.addEventListener("unhandledrejection", onRejection);
  });
  afterEach(() => {
    window.removeEventListener("unhandledrejection", onRejection);
  });

  test.each([
    ["malformed JSON", "{not json"],
    ["an empty object", "{}"],
    [
      "a missing state",
      JSON.stringify({ redirectUri: "x", codeVerifier: "v" }),
    ],
    ["a non-object", "42"],
  ])(
    "%s shows the failure, deletes the token and starts no completion",
    async (_name, value) => {
      window.sessionStorage.setItem(SIGN_IN_SESSION_KEY, value);
      pendingInvitation.save(TOKEN);
      renderCallback({ url: "/callback?code=abc&state=xyz" });
      await settle();
      expect(
        screen.getByText("The sign-in could not be completed. Sign in again."),
      ).toBeTruthy();
      expect(
        screen.getByRole("button", { name: "Sign in with Delino" }),
      ).toBeTruthy();
      expect(window.location.pathname).toBe("/callback");
      expect(pendingInvitation.read()).toBeUndefined();
      expect(completions).toBe(0);
      expect(rejections).toEqual([]);
    },
  );

  test("shows the failure under StrictMode too", async () => {
    window.sessionStorage.setItem(SIGN_IN_SESSION_KEY, "{not json");
    pendingInvitation.save(TOKEN);
    renderCallback({ strict: true });
    await settle();
    expect(recoveryNotice()).toBeTruthy();
    expect(pendingInvitation.read()).toBeUndefined();
  });

  test("the sign-in button starts the sign-in redirect", async () => {
    window.sessionStorage.setItem(SIGN_IN_SESSION_KEY, "{not json");
    renderCallback();
    await settle();
    await act(async () => {
      within(recoveryNotice())
        .getByRole("button", { name: "Sign in with Delino" })
        .click();
    });
    expect(signIns).toBe(1);
  });

  test("shows the sign-in failure alert only after a failed sign-in start", async () => {
    window.sessionStorage.setItem(SIGN_IN_SESSION_KEY, "{not json");
    renderCallback();
    await settle();
    expect(screen.queryByText(SIGN_IN_FAILURE_MESSAGE)).toBeNull();
    cleanup();
    renderCallback({ auth: { signInFailed: true } });
    await settle();
    expect(screen.getByText(SIGN_IN_FAILURE_MESSAGE)).toBeTruthy();
    expect(recoveryNotice()).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Sign in with Delino" }),
    ).toBeTruthy();
  });
});

describe("readLogtoSignInRedirect", () => {
  const location = { origin: "https://accounts.test", pathname: "/callback" };
  const session = (redirectUri: string) =>
    JSON.stringify({ redirectUri, codeVerifier: "v", state: "s" });
  const storage = (items: Record<string, string>) => ({
    getItem: (key: string) => items[key] ?? null,
  });

  test("is none without a stored session", () => {
    expect(readLogtoSignInRedirect("app", location, storage({}))).toBe(
      LogtoSignInRedirect.None,
    );
    expect(readLogtoSignInRedirect("app", location, undefined)).toBe(
      LogtoSignInRedirect.None,
    );
  });

  test("is pending when the session redirect URI is this address", () => {
    const items = {
      "logto:app:signInSession": session("https://accounts.test/callback"),
    };
    expect(readLogtoSignInRedirect("app", location, storage(items))).toBe(
      LogtoSignInRedirect.Pending,
    );
  });

  test("reads the legacy session key", () => {
    const items = { "logto:app": session("https://accounts.test/callback") };
    expect(readLogtoSignInRedirect("app", location, storage(items))).toBe(
      LogtoSignInRedirect.Pending,
    );
  });

  test("ignores the session of another application", () => {
    const items = {
      "logto:other:signInSession": session("https://accounts.test/callback"),
    };
    expect(readLogtoSignInRedirect("app", location, storage(items))).toBe(
      LogtoSignInRedirect.None,
    );
  });

  test("is none when the session belongs to another address", () => {
    const items = {
      "logto:app:signInSession": session("https://accounts.test/other"),
    };
    expect(readLogtoSignInRedirect("app", location, storage(items))).toBe(
      LogtoSignInRedirect.None,
    );
  });

  test("is unreadable for malformed JSON and for an invalid shape", () => {
    const invalid = [
      "{not json",
      "{}",
      "null",
      "[]",
      '"text"',
      JSON.stringify({ redirectUri: "https://accounts.test/callback" }),
      JSON.stringify({
        redirectUri: "https://accounts.test/callback",
        codeVerifier: 1,
        state: "s",
      }),
      JSON.stringify({ redirectUri: 1, codeVerifier: "v", state: "s" }),
    ];
    for (const value of invalid) {
      expect(
        readLogtoSignInRedirect(
          "app",
          location,
          storage({ "logto:app:signInSession": value }),
        ),
      ).toBe(LogtoSignInRedirect.Unreadable);
    }
  });

  test("is unreadable for an invalid session under the legacy key", () => {
    expect(
      readLogtoSignInRedirect("app", location, storage({ "logto:app": "{" })),
    ).toBe(LogtoSignInRedirect.Unreadable);
  });

  test("is none when the storage throws", () => {
    const throwing = {
      getItem: () => {
        throw new Error("blocked");
      },
    };
    expect(readLogtoSignInRedirect("app", location, throwing)).toBe(
      LogtoSignInRedirect.None,
    );
  });
});
