// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, useLogto } from "@logto/react";
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rstest,
  test,
} from "@rstest/core";
import { act, cleanup, render, screen } from "@testing-library/react";
import { StrictMode } from "react";
import { LogtoCallbackPage } from "./pages/callback.tsx";
import { RouterProvider } from "./router.tsx";

const LOGTO_CONFIG = { endpoint: "https://logto.test", appId: "app" };
const SIGN_IN_SESSION_KEY = "logto:app:signInSession";
const LEGACY_SESSION_KEY = "logto:app";
const ID_TOKEN_KEY = "logto:app:idToken";
const CALLBACK_WITH_QUERY = "/callback?code=x&state=y";

function storeSignInSession(path = "/callback", key = SIGN_IN_SESSION_KEY) {
  window.sessionStorage.setItem(
    key,
    JSON.stringify({
      redirectUri: window.location.origin + path,
      codeVerifier: "v",
      state: "y",
    }),
  );
}

async function renderCallback({
  path = CALLBACK_WITH_QUERY,
  signedIn = false,
  onCallback = async () => {},
}: {
  path?: string;
  signedIn?: boolean;
  onCallback?: () => Promise<void>;
} = {}) {
  window.history.replaceState(null, "", path);
  const replace = rstest.spyOn(window.history, "replaceState");
  const push = rstest.spyOn(window.history, "pushState");
  const complete = rstest.fn(async () => {
    await onCallback();
    window.sessionStorage.removeItem(SIGN_IN_SESSION_KEY);
    window.sessionStorage.removeItem(LEGACY_SESSION_KEY);
  });

  class FakeLogtoClient {
    isAuthenticated = async () => signedIn;
    isSignInRedirected = async () =>
      window.sessionStorage.getItem(SIGN_IN_SESSION_KEY) !== null ||
      window.sessionStorage.getItem(LEGACY_SESSION_KEY) !== null;
    handleSignInCallback = complete;
  }

  await act(async () => {
    render(
      <StrictMode>
        <LogtoProvider
          config={LOGTO_CONFIG}
          LogtoClientClass={FakeLogtoClient as never}
        >
          <RouterProvider>
            <LogtoCallbackPage appId={LOGTO_CONFIG.appId} />
          </RouterProvider>
        </LogtoProvider>
      </StrictMode>,
    );
  });
  return { replace, push, complete };
}

beforeEach(() => {
  window.sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  rstest.restoreAllMocks();
});

describe("Logto callback", () => {
  test.each([
    { path: "/callback", signedIn: false },
    { path: "/callback", signedIn: true },
    { path: CALLBACK_WITH_QUERY, signedIn: false },
    { path: CALLBACK_WITH_QUERY, signedIn: true },
  ])(
    "replaces $path once under StrictMode (signed in: $signedIn)",
    async ({ path, signedIn }) => {
      const { replace, push, complete } = await renderCallback({
        path,
        signedIn,
      });

      expect(window.location.pathname + window.location.search).toBe("/");
      expect(replace.mock.calls).toEqual([[null, "", "/"]]);
      expect(push).not.toHaveBeenCalled();
      expect(complete).not.toHaveBeenCalled();
    },
  );

  test("leaves the callback when the stored session names a different address", async () => {
    storeSignInSession("/other");
    const { replace, complete } = await renderCallback();

    expect(window.location.pathname).toBe("/");
    expect(replace.mock.calls).toEqual([[null, "", "/"]]);
    expect(complete).not.toHaveBeenCalled();
  });

  test.each([SIGN_IN_SESSION_KEY, LEGACY_SESSION_KEY])(
    "waits for the callback to complete when a session is stored in %s",
    async (key) => {
      storeSignInSession("/callback", key);
      let finish!: () => void;
      const completion = new Promise<void>((resolve) => {
        finish = resolve;
      });
      const { replace, push, complete } = await renderCallback({
        onCallback: () => completion,
      });

      expect(complete).toHaveBeenCalledTimes(1);
      expect(window.location.pathname + window.location.search).toBe(
        CALLBACK_WITH_QUERY,
      );
      expect(replace).not.toHaveBeenCalled();
      await act(async () => finish());

      expect(window.location.pathname + window.location.search).toBe("/");
      expect(replace.mock.calls).toEqual([[null, "", "/"]]);
      expect(push).not.toHaveBeenCalled();
    },
  );

  test("shows the error notice and stays on a failed callback", async () => {
    storeSignInSession();
    const { replace, push, complete } = await renderCallback({
      onCallback: async () => {
        throw new Error("callback failed");
      },
    });

    expect(complete).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("alert").textContent).toContain(
      "Something went wrong",
    );
    expect(window.location.pathname).toBe("/callback");
    expect(replace).not.toHaveBeenCalled();
    expect(push).not.toHaveBeenCalled();
  });
});

describe("Logto callback with unreadable session storage", () => {
  const unhandled: unknown[] = [];
  const onUnhandled = (reason: unknown) => unhandled.push(reason);
  const session = {
    redirectUri: "https://console.test/callback",
    codeVerifier: "v",
    state: "s",
  };
  const unreadableCases = [
    { name: "invalid JSON", value: "{not json" },
    { name: "an object without fields", value: "{}" },
    { name: "null", value: "null" },
    ...Object.keys(session).flatMap((key) => {
      const fields: Record<string, unknown> = { ...session };
      delete fields[key];
      return [
        { name: `missing ${key}`, value: JSON.stringify(fields) },
        {
          name: `non-string ${key}`,
          value: JSON.stringify({ ...session, [key]: 1 }),
        },
      ];
    }),
  ];

  function AuthStatus() {
    const { isAuthenticated, isLoading } = useLogto();
    if (isLoading) return null;
    return <span>{isAuthenticated ? "signed in" : "signed out"}</span>;
  }

  beforeEach(() => {
    unhandled.length = 0;
    process.on("unhandledRejection", onUnhandled);
  });

  afterEach(() => {
    process.off("unhandledRejection", onUnhandled);
    window.localStorage.removeItem(ID_TOKEN_KEY);
  });

  for (const storageKey of [SIGN_IN_SESSION_KEY, LEGACY_SESSION_KEY]) {
    for (const signedIn of [false, true]) {
      test.each(unreadableCases)(
        `shows one error notice for $name in ${storageKey} under StrictMode (signed in: ${signedIn})`,
        async ({ value }) => {
          window.sessionStorage.setItem(storageKey, value);
          // The SDK uses the presence of an ID token to restore authentication.
          if (signedIn) window.localStorage.setItem(ID_TOKEN_KEY, "fixture");
          window.history.replaceState(null, "", CALLBACK_WITH_QUERY);
          const replace = rstest.spyOn(window.history, "replaceState");
          const push = rstest.spyOn(window.history, "pushState");

          await act(async () => {
            render(
              <StrictMode>
                <LogtoProvider config={LOGTO_CONFIG}>
                  <AuthStatus />
                  <RouterProvider>
                    <LogtoCallbackPage appId={LOGTO_CONFIG.appId} />
                  </RouterProvider>
                </LogtoProvider>
              </StrictMode>,
            );
          });
          expect(
            screen.getByText(signedIn ? "signed in" : "signed out"),
          ).toBeDefined();
          expect(screen.getByRole("alert").textContent).toBe(
            "Something went wrong. Try again.",
          );
          expect(window.location.pathname + window.location.search).toBe(
            CALLBACK_WITH_QUERY,
          );
          expect(replace).not.toHaveBeenCalled();
          expect(push).not.toHaveBeenCalled();
          expect(window.sessionStorage.getItem(storageKey)).toBe(value);
          // Let an unhandled SDK rejection reach the process before checking.
          await new Promise((resolve) => setImmediate(resolve));
          expect(unhandled).toEqual([]);
        },
      );
    }
  }
});
