// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider } from "@logto/react";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { act, cleanup, render, screen } from "@testing-library/react";
import { StrictMode, useEffect } from "react";
import { isLogtoSignInPending, LogtoBridge, useAuth } from "./auth.tsx";
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
  function renderCallback(options: { strict?: boolean } = {}) {
    window.history.replaceState(null, "", "/callback");
    const navigations: string[] = [];
    const onNavigate = (_from: Route, to: Route) => navigations.push(to.id);
    const page = (
      <LogtoProvider
        config={{ endpoint: "https://logto.test", appId: APP_ID }}
        LogtoClientClass={FakeLogtoClient as never}
      >
        <RouterProvider onNavigate={onNavigate}>
          <LogtoCallbackPage appId={APP_ID} />
        </RouterProvider>
      </LogtoProvider>
    );
    render(options.strict ? <StrictMode>{page}</StrictMode> : page);
    return { navigations };
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
    window.history.replaceState(null, "", "/callback?code=x&state=y");
    renderCallback();
    await settle();
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
});

describe("isLogtoSignInPending", () => {
  const location = { origin: "https://console.test", pathname: "/callback" };
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
      "logto:app:signInSession": session("https://console.test/callback"),
    };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(true);
  });

  test("reads the legacy session key", () => {
    const items = { "logto:app": session("https://console.test/callback") };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(true);
  });

  test("ignores the session of another application", () => {
    const items = {
      "logto:other:signInSession": session("https://console.test/callback"),
    };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(false);
  });

  test("is false when the session belongs to another address", () => {
    const items = {
      "logto:app:signInSession": session("https://console.test/other"),
    };
    expect(isLogtoSignInPending("app", location, storage(items))).toBe(false);
  });

  test("is true for a session that is not readable, so the hook reports it", () => {
    expect(
      isLogtoSignInPending(
        "app",
        location,
        storage({ "logto:app:signInSession": "{not json" }),
      ),
    ).toBe(true);
    expect(
      isLogtoSignInPending(
        "app",
        location,
        storage({ "logto:app:signInSession": "{}" }),
      ),
    ).toBe(true);
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
