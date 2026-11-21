// SPDX-License-Identifier: Apache-2.0
import { LogtoProvider } from "@logto/react";
import { afterEach, describe, expect, test } from "@rstest/core";
import { act, cleanup, render } from "@testing-library/react";
import { useEffect } from "react";
import { LogtoBridge, useAuth } from "./auth.tsx";

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
