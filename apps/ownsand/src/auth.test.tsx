// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider } from "@logto/react";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { act, cleanup, render, screen } from "@testing-library/react";
import { StrictMode, useEffect, useState } from "react";
import {
  LogtoBridge,
  logtoSignInSessionState,
  SignInSessionState,
  useAuth,
} from "./auth.tsx";
import { pendingInvitation } from "./invitation.ts";
import { LogtoCallbackPage } from "./pages/callback.tsx";
import { type Route, RouterProvider } from "./router.tsx";

const APP_ID = "app";
const SIGN_IN_SESSION_KEY = `logto:${APP_ID}:signInSession`;

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

/** A Logto client that is signed in and returns a token after a delay. */
class FakeLogtoClient {
  // Like `@logto/client`: a redirect is pending only with a stored session.
  isSignInRedirected = async () =>
    window.sessionStorage.getItem(SIGN_IN_SESSION_KEY) !== null;
  handleSignInCallback = async () => {
    await handleSignInCallback();
    window.sessionStorage.removeItem(SIGN_IN_SESSION_KEY);
  };
  isAuthenticated = async () => true;
  getAccessToken = async () => {
    await new Promise((resolve) => setTimeout(resolve, 5));
    return "token";
  };
  getIdTokenClaims = async () => ({ sub: "user-1" });
  getRefreshToken = async () => undefined;
  getAccessTokenClaims = async () => ({});
  getOrganizationToken = async () => undefined;
  getOrganizationTokenClaims = async () => ({});
  getIdToken = async () => undefined;
  signIn = async () => {};
  signOut = async () => {};
  fetchUserInfo = async () => ({});
  clearAccessToken = async () => {};
  clearAllTokens = async () => {};
}

beforeEach(() => {
  window.sessionStorage.clear();
  handleSignInCallback = async () => {};
});

afterEach(() => {
  cleanup();
});

describe("LogtoBridge", () => {
  test("a token request does not unmount the page that made it", async () => {
    let mounts = 0;
    // Like the console routes: nothing below renders while `isLoading`.
    function Page() {
      const auth = useAuth();
      // Runs once per mount, like a query that starts when its page mounts.
      // biome-ignore lint/correctness/useExhaustiveDependencies: mount only
      useEffect(() => {
        mounts += 1;
        void auth.getAccessToken();
      }, []);
      return <p>page</p>;
    }
    function Gate() {
      const auth = useAuth();
      return auth.isLoading ? null : <Page />;
    }
    render(
      <LogtoProvider
        config={{ endpoint: "https://logto.test", appId: "app" }}
        LogtoClientClass={FakeLogtoClient as never}
      >
        <LogtoBridge>
          <Gate />
        </LogtoBridge>
      </LogtoProvider>,
    );
    // Wait in short steps: React applies state updates when an `act` scope
    // ends, so one long scope would hide the unmount and mount cycle.
    for (let step = 0; step < 20; step += 1) {
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 10));
      });
    }
    expect(mounts).toBe(1);
  });
});

describe("LogtoCallbackPage", () => {
  /** Mounts the page like the console: under a Logto provider and the router. */
  function renderCallback(
    options: { strict?: boolean; url?: string; realClient?: boolean } = {},
  ) {
    window.history.replaceState(null, "", options.url ?? "/callback");
    const navigations: string[] = [];
    // The address that the first render sees, before any effect navigates.
    const seenAtMount: string[] = [];
    function MountProbe() {
      const [first] = useState(
        () => window.location.pathname + window.location.search,
      );
      seenAtMount.push(first);
      return null;
    }
    const onNavigate = (_from: Route, to: Route) => navigations.push(to.id);
    const page = (
      <LogtoProvider
        config={{ endpoint: "https://logto.test", appId: APP_ID }}
        LogtoClientClass={
          options.realClient ? undefined : (FakeLogtoClient as never)
        }
      >
        <RouterProvider onNavigate={onNavigate}>
          <MountProbe />
          <LogtoCallbackPage appId={APP_ID} />
        </RouterProvider>
      </LogtoProvider>
    );
    render(options.strict ? <StrictMode>{page}</StrictMode> : page);
    return { navigations, seenAtMount };
  }

  async function settle(ms = 50) {
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, ms));
    });
  }

  test("leaves the route for the organization list when no sign-in is in progress", async () => {
    const { navigations } = renderCallback();
    await settle();
    expect(window.location.pathname).toBe("/");
    expect(navigations).toEqual(["organizations"]);
  });

  test("replaces the history entry instead of adding one", async () => {
    const before = window.history.length;
    renderCallback();
    await settle();
    expect(window.history.length).toBe(before);
  });

  test("leaves the route once under StrictMode", async () => {
    const { navigations } = renderCallback({ strict: true });
    await settle();
    expect(window.location.pathname).toBe("/");
    expect(navigations).toEqual(["organizations"]);
  });

  test("leaves the route for a pending invitation when one is stored", async () => {
    pendingInvitation.save("osi_pending");
    renderCallback();
    await settle();
    expect(window.location.pathname).toBe("/invitations/accept");
    expect(pendingInvitation.read()).toBe("osi_pending");
  });

  test("ignores a code and a state without a stored sign-in session", async () => {
    const { navigations, seenAtMount } = renderCallback({
      url: "/callback?code=x&state=y",
    });
    // The page mounted with the query, so the test exercises the redirect
    // parameters and not an address that the helper had already reset.
    expect(seenAtMount[0]).toBe("/callback?code=x&state=y");
    await settle();
    expect(navigations).toEqual(["organizations"]);
    expect(window.location.pathname).toBe("/");
  });

  test("navigates only after the sign-in callback resolves, and once", async () => {
    storeSignInSession();
    let release: () => void = () => {};
    handleSignInCallback = () =>
      new Promise<void>((resolve) => {
        release = resolve;
      });
    const { navigations } = renderCallback();
    await settle();
    expect(window.location.pathname).toBe("/callback");
    expect(navigations).toEqual([]);
    await act(async () => {
      release();
    });
    await settle();
    expect(window.location.pathname).toBe("/");
    expect(navigations).toEqual(["organizations"]);
  });

  test("completes a redirect under StrictMode and navigates once", async () => {
    storeSignInSession();
    pendingInvitation.save("osi_pending");
    const { navigations } = renderCallback({ strict: true });
    await settle();
    expect(window.location.pathname).toBe("/invitations/accept");
    expect(navigations).toEqual(["invitation"]);
    expect(pendingInvitation.read()).toBe("osi_pending");
  });

  test("shows the error notice and stays when the callback fails", async () => {
    storeSignInSession();
    handleSignInCallback = async () => {
      throw new Error("callback failed");
    };
    const { navigations } = renderCallback();
    await settle();
    expect(screen.getByRole("alert").textContent).toContain(
      "Something went wrong",
    );
    expect(window.location.pathname).toBe("/callback");
    expect(navigations).toEqual([]);
  });

  test("drops the pending invitation secret when the callback fails", async () => {
    storeSignInSession();
    pendingInvitation.save("osi_pending");
    handleSignInCallback = async () => {
      throw new Error("callback failed");
    };
    renderCallback({ strict: true });
    await settle();
    expect(screen.getByRole("alert").textContent).toContain(
      "Something went wrong",
    );
    expect(pendingInvitation.read()).toBeUndefined();
  });

  describe("with a stored sign-in session that the SDK cannot read", () => {
    const unhandled: unknown[] = [];
    const onUnhandled = (reason: unknown) => unhandled.push(reason);
    beforeEach(() => {
      unhandled.length = 0;
      process.on("unhandledRejection", onUnhandled);
    });
    afterEach(() => {
      process.off("unhandledRejection", onUnhandled);
    });

    const unreadable: Array<[string, string]> = [
      ["text that is not JSON", "{not json"],
      ["an object without fields", "{}"],
      [
        "a session without a redirect URI",
        JSON.stringify({ codeVerifier: "v", state: "s" }),
      ],
      [
        "a session with a field that is not a string",
        JSON.stringify({
          redirectUri: "http://localhost/callback",
          codeVerifier: 1,
          state: "s",
        }),
      ],
    ];
    for (const [name, value] of unreadable) {
      test(`shows the error notice, stays, and drops the invitation secret for ${name}`, async () => {
        window.sessionStorage.setItem(SIGN_IN_SESSION_KEY, value);
        pendingInvitation.save("osi_pending");
        const { navigations } = renderCallback({
          url: "/callback?code=x&state=y",
          realClient: true,
          strict: true,
        });
        await settle(100);
        expect(screen.getByRole("alert").textContent).toContain(
          "Something went wrong",
        );
        expect(window.location.pathname).toBe("/callback");
        expect(navigations).toEqual([]);
        expect(pendingInvitation.read()).toBeUndefined();
        // Let a rejection that the SDK hook would raise reach the process.
        await settle(100);
        expect(unhandled).toEqual([]);
      });
    }
  });
});

describe("logtoSignInSessionState", () => {
  const location = { origin: "https://console.test", pathname: "/callback" };
  const session = (redirectUri: string) =>
    JSON.stringify({ redirectUri, codeVerifier: "v", state: "s" });
  const state = logtoSignInSessionState;
  const storage = (items: Record<string, string>) => ({
    getItem: (key: string) => items[key] ?? null,
  });

  test("is false without a stored session", () => {
    expect(state("app", location, storage({}))).toBe(SignInSessionState.None);
    expect(state("app", location, undefined)).toBe(SignInSessionState.None);
  });

  test("is true when the session redirect URI is this address", () => {
    const items = {
      "logto:app:signInSession": session("https://console.test/callback"),
    };
    expect(state("app", location, storage(items))).toBe(
      SignInSessionState.Pending,
    );
  });

  test("reads the legacy session key", () => {
    const items = { "logto:app": session("https://console.test/callback") };
    expect(state("app", location, storage(items))).toBe(
      SignInSessionState.Pending,
    );
  });

  test("ignores the session of another application", () => {
    const items = {
      "logto:other:signInSession": session("https://console.test/callback"),
    };
    expect(state("app", location, storage(items))).toBe(
      SignInSessionState.None,
    );
  });

  test("is false when the session belongs to another address", () => {
    const items = {
      "logto:app:signInSession": session("https://console.test/other"),
    };
    expect(state("app", location, storage(items))).toBe(
      SignInSessionState.None,
    );
  });

  test("is invalid for a session that the SDK cannot read", () => {
    const unreadable = [
      "{not json",
      "{}",
      "null",
      "[]",
      '"text"',
      JSON.stringify({ codeVerifier: "v", state: "s" }),
      JSON.stringify({
        redirectUri: "https://console.test/callback",
        state: "s",
      }),
      JSON.stringify({
        redirectUri: "https://console.test/callback",
        codeVerifier: "v",
      }),
      JSON.stringify({
        redirectUri: "https://console.test/callback",
        codeVerifier: 1,
        state: "s",
      }),
    ];
    for (const value of unreadable) {
      expect(
        state("app", location, storage({ "logto:app:signInSession": value })),
      ).toBe(SignInSessionState.Invalid);
      expect(state("app", location, storage({ "logto:app": value }))).toBe(
        SignInSessionState.Invalid,
      );
    }
  });

  test("is none for an empty stored value", () => {
    expect(
      state("app", location, storage({ "logto:app:signInSession": "" })),
    ).toBe(SignInSessionState.None);
  });

  test("is false when the storage throws", () => {
    const throwing = {
      getItem: () => {
        throw new Error("blocked");
      },
    };
    expect(state("app", location, throwing)).toBe(SignInSessionState.None);
  });
});

describe("sign-out", () => {
  test("drops the pending invitation secret", async () => {
    pendingInvitation.save("osi_pending");
    let signOut: () => Promise<void> = async () => {};
    function Capture() {
      signOut = useAuth().signOut;
      return null;
    }
    render(
      <LogtoProvider
        config={{ endpoint: "https://logto.test", appId: APP_ID }}
        LogtoClientClass={FakeLogtoClient as never}
      >
        <LogtoBridge>
          <Capture />
        </LogtoBridge>
      </LogtoProvider>,
    );
    await act(async () => {
      await signOut();
    });
    expect(pendingInvitation.read()).toBeUndefined();
  });
});
