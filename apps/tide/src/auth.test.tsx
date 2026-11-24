// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider, useLogto } from "@logto/react";
import { afterEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { useEffect } from "react";
import {
  LogtoBridge,
  logtoSignInSessionState,
  SignInSessionState,
  useAuth,
} from "./auth.tsx";

/** A Logto client that is signed in and returns a token after a delay. */
class FakeLogtoClient {
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

afterEach(() => {
  cleanup();
});

describe("LogtoBridge", () => {
  test("ignores an existing provider error before and after a successful sign-in attempt", async () => {
    const providerError = new Error("Earlier provider error");
    class SignedOutClient extends FakeLogtoClient {
      isAuthenticated = async () => false;
      fetchUserInfo = async () => {
        throw providerError;
      };
    }
    function Page() {
      const auth = useAuth();
      const { error, fetchUserInfo } = useLogto();
      if (auth.isLoading) return null;
      return (
        <>
          <button type="button" onClick={() => void fetchUserInfo()}>
            Seed provider error
          </button>
          <button type="button" onClick={() => void auth.signIn()}>
            Start sign-in
          </button>
          <p>{error?.message}</p>
          {auth.signInFailed ? <p role="alert">Sign-in failed</p> : null}
        </>
      );
    }
    render(
      <LogtoProvider
        config={{ endpoint: "https://logto.test", appId: "app" }}
        LogtoClientClass={SignedOutClient as never}
      >
        <LogtoBridge>
          <Page />
        </LogtoBridge>
      </LogtoProvider>,
    );

    fireEvent.click(
      await screen.findByRole("button", { name: "Seed provider error" }),
    );
    await screen.findByText(providerError.message);
    expect(screen.queryByRole("alert")).toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Start sign-in" }));
    });
    expect(screen.getByText(providerError.message)).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

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

describe("logtoSignInSessionState", () => {
  const location = { origin: "https://console.test", pathname: "/callback" };
  const currentKey = "logto:app:signInSession";
  const legacyKey = "logto:app";
  const sessionFor = (pathname: string) =>
    JSON.stringify({
      redirectUri: location.origin + pathname,
      codeVerifier: "v",
      state: "s",
    });
  const pending = sessionFor("/callback");

  const cases: Array<{
    name: string;
    items: Record<string, string>;
    expected: SignInSessionState;
  }> = [
    { name: "no entry", items: {}, expected: SignInSessionState.None },
    {
      name: "empty current key with an unreadable legacy entry",
      items: { [currentKey]: "", [legacyKey]: "null" },
      expected: SignInSessionState.None,
    },
    {
      name: "entry for another application",
      items: { "logto:other:signInSession": "null" },
      expected: SignInSessionState.None,
    },
    {
      name: "readable legacy entry for another path",
      items: { [legacyKey]: sessionFor("/other") },
      expected: SignInSessionState.None,
    },
    {
      name: "readable current entry for this path",
      items: { [currentKey]: pending, [legacyKey]: "null" },
      expected: SignInSessionState.Pending,
    },
    {
      name: "readable legacy entry for this path",
      items: { [legacyKey]: pending },
      expected: SignInSessionState.Pending,
    },
    {
      name: "unreadable current entry with a pending legacy entry",
      items: { [currentKey]: "null", [legacyKey]: pending },
      expected: SignInSessionState.Invalid,
    },
    {
      name: "legacy entry with invalid JSON",
      items: { [legacyKey]: "{not json" },
      expected: SignInSessionState.Invalid,
    },
    {
      name: "empty object",
      items: { [currentKey]: "{}" },
      expected: SignInSessionState.Invalid,
    },
    {
      name: "missing redirect URI",
      items: { [currentKey]: '{"codeVerifier":"v","state":"s"}' },
      expected: SignInSessionState.Invalid,
    },
    {
      name: "numeric code verifier",
      items: {
        [currentKey]:
          '{"redirectUri":"https://console.test/callback","codeVerifier":1,"state":"s"}',
      },
      expected: SignInSessionState.Invalid,
    },
  ];

  test.each(cases)("classifies $name as $expected", ({ items, expected }) => {
    expect(
      logtoSignInSessionState("app", location, {
        getItem: (key) => items[key] ?? null,
      }),
    ).toBe(expected);
  });

  test("treats inaccessible storage as no pending session", () => {
    expect(
      logtoSignInSessionState("app", location, {
        getItem: () => {
          throw new Error("Storage is inaccessible");
        },
      }),
    ).toBe(SignInSessionState.None);
  });
});
