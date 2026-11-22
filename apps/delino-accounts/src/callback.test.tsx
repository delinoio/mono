// SPDX-License-Identifier: Apache-2.0
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";
import { type ReactNode, StrictMode, useEffect, useState } from "react";
import { Accounts } from "./App.tsx";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { pendingInvitation } from "./invitation.ts";
import { TEST_CONFIG } from "./testing/render.tsx";

/**
 * The page reads the Logto callback result through `useHandleSignInCallback`.
 * A real redirect round trip needs a Logto tenant, so these tests replace the
 * hook with a controllable outcome and keep the rest of the app real.
 */
const callbackOutcome: {
  error: Error | undefined;
  succeed: boolean;
  calls: number;
} = { error: undefined, succeed: false, calls: 0 };

rs.mock("@logto/react", () => ({
  LogtoProvider: ({ children }: { children: ReactNode }) => children,
  useLogto: () => ({}),
  useHandleSignInCallback: (onSuccess: () => void) => {
    const [done, setDone] = useState(false);
    useEffect(() => {
      if (callbackOutcome.succeed && !done) {
        callbackOutcome.calls++;
        setDone(true);
        onSuccess();
      }
    }, [done, onSuccess]);
    return {
      isLoading: !callbackOutcome.error && !done,
      error: callbackOutcome.error,
    };
  },
}));

const TOKEN = "bai_TESTTOKEN";

const AUTH: Auth = {
  isLoading: false,
  isAuthenticated: false,
  userId: undefined,
  signIn: async () => {},
  signOut: async () => {},
  getAccessToken: async () => undefined,
};

function renderCallback(strict: boolean) {
  window.history.replaceState(null, "", "/callback?code=abc&state=xyz");
  const app = (
    <AuthContextProvider value={AUTH}>
      <Accounts
        config={TEST_CONFIG}
        queryClient={new QueryClient()}
        checkout={undefined}
      />
    </AuthContextProvider>
  );
  render(strict ? <StrictMode>{app}</StrictMode> : app);
}

async function flush() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 30));
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  callbackOutcome.error = undefined;
  callbackOutcome.succeed = false;
  callbackOutcome.calls = 0;
});

afterEach(() => {
  cleanup();
});

describe("sign-in callback and the pending invitation token", () => {
  test.each([false, true])(
    "a failed return deletes the stored token (StrictMode %s)",
    async (strict) => {
      pendingInvitation.save(TOKEN);
      callbackOutcome.error = new Error("access_denied");
      renderCallback(strict);
      await flush();
      expect(screen.getByText("Something went wrong. Try again.")).toBeTruthy();
      expect(pendingInvitation.read()).toBeUndefined();
      expect(window.location.pathname).toBe("/callback");
    },
  );

  test("a loading return keeps the stored token", async () => {
    pendingInvitation.save(TOKEN);
    renderCallback(false);
    await flush();
    expect(pendingInvitation.read()).toBe(TOKEN);
  });

  test.each([false, true])(
    "a successful return keeps the token and opens the invitation (StrictMode %s)",
    async (strict) => {
      pendingInvitation.save(TOKEN);
      callbackOutcome.succeed = true;
      renderCallback(strict);
      await flush();
      expect(callbackOutcome.calls).toBeGreaterThan(0);
      expect(window.location.pathname).toBe("/billing/invitations/accept");
      expect(pendingInvitation.read()).toBe(TOKEN);
    },
  );

  test("a successful return without a token opens billing", async () => {
    callbackOutcome.succeed = true;
    renderCallback(false);
    await flush();
    expect(window.location.pathname).toBe("/billing");
  });
});
