// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { QueryClient } from "@tanstack/react-query";
import { Console } from "./App.tsx";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { pendingInvitation } from "./invitation.ts";
import { MOCK_INVITATION_SECRET, MOCK_USER } from "./mock/mock-api.ts";
import {
  holdingTransport,
  renderConsole,
  settle,
  testConfig,
} from "./test-support.tsx";

// The earlier link is a real mock invitation, so that its late success is a
// success of the mock control plane. The newer link needs no valid secret:
// the tests check which secret is stored and sent.
const SECRET_A = MOCK_INVITATION_SECRET;
const SECRET_B = "osi_newer_B";

function authValue(signedIn: boolean): Auth {
  return {
    isLoading: false,
    isAuthenticated: signedIn,
    userId: signedIn ? MOCK_USER : undefined,
    signIn: async () => {},
    signInFailed: false,
    signOut: async () => {},
    getAccessToken: async () => "token",
  };
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

/**
 * Starts an accept request for invitation A and holds it, opens link B on the
 * same page, then signs the user out and presses "Sign in to accept" for B, so
 * that B's secret waits in `sessionStorage` for its sign-in redirect while A's
 * request is still in flight.
 */
async function startStaleAcceptWithNewerSignIn(queryClient?: QueryClient) {
  const held = holdingTransport(["AcceptInvitation"]);
  window.history.replaceState(
    null,
    "",
    `/invitations/accept#secret=${SECRET_A}`,
  );
  const client =
    queryClient ??
    new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
  const tree = (signedIn: boolean) => (
    <AuthContextProvider value={authValue(signedIn)}>
      <Console
        config={testConfig}
        transport={held.transport}
        queryClient={client}
      />
    </AuthContextProvider>
  );
  const view = render(tree(true));
  await act(async () => {
    fireEvent.click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
  });
  expect(held.calls.AcceptInvitation).toHaveLength(1);
  expect(
    (held.calls.AcceptInvitation[0].input as { invitationSecret: string })
      .invitationSecret,
  ).toBe(SECRET_A);
  await act(async () => {
    window.location.hash = `secret=${SECRET_B}`;
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  view.rerender(tree(false));
  await act(async () => {
    fireEvent.click(
      await screen.findByRole("button", { name: "Sign in to accept" }),
    );
  });
  expect(pendingInvitation.read()).toBe(SECRET_B);
  return { held, client };
}

/** Loads the invitation page as the sign-in return does and accepts. */
async function acceptAfterSignInReturn(
  held: ReturnType<typeof holdingTransport>,
) {
  cleanup();
  renderConsole("/invitations/accept", { transport: held.transport });
  await act(async () => {
    fireEvent.click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
  });
  const calls = held.calls.AcceptInvitation;
  expect(calls).toHaveLength(2);
  expect(
    (calls[1].input as { invitationSecret: string }).invitationSecret,
  ).toBe(SECRET_B);
  calls[1].settle();
  await settle(60);
}

describe("a stale invitation acceptance", () => {
  test("keeps the secret of a newer link when the earlier request succeeds", async () => {
    const { held } = await startStaleAcceptWithNewerSignIn();
    await act(async () => {
      held.calls.AcceptInvitation[0].settle();
    });
    await settle(60);
    expect(pendingInvitation.read()).toBe(SECRET_B);
    expect(window.location.pathname).toBe("/invitations/accept");
    await acceptAfterSignInReturn(held);
    expect(pendingInvitation.read()).toBeUndefined();
  });

  test("keeps the secret of a newer link when the earlier request fails", async () => {
    const { held } = await startStaleAcceptWithNewerSignIn();
    await act(async () => {
      held.calls.AcceptInvitation[0].settle(
        new ConnectError("down", Code.Unavailable),
      );
    });
    await settle(60);
    expect(pendingInvitation.read()).toBe(SECRET_B);
    await acceptAfterSignInReturn(held);
    expect(pendingInvitation.read()).toBeUndefined();
  });

  test("still invalidates the organization queries on a stale success", async () => {
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    let invalidations = 0;
    const invalidate = client.invalidateQueries.bind(client);
    client.invalidateQueries = ((...args: Parameters<typeof invalidate>) => {
      invalidations += 1;
      return invalidate(...args);
    }) as typeof client.invalidateQueries;
    const { held } = await startStaleAcceptWithNewerSignIn(client);
    expect(invalidations).toBe(0);
    await act(async () => {
      held.calls.AcceptInvitation[0].settle();
    });
    await settle(60);
    // One invalidation for each of the member and organization services.
    expect(invalidations).toBe(2);
    expect(window.location.pathname).toBe("/invitations/accept");
  });
});

describe("the current invitation acceptance", () => {
  test("deletes its stored secret when the request fails", async () => {
    const held = holdingTransport(["AcceptInvitation"]);
    pendingInvitation.save(SECRET_A);
    renderConsole("/invitations/accept", { transport: held.transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
    });
    expect(pendingInvitation.read()).toBe(SECRET_A);
    await act(async () => {
      held.calls.AcceptInvitation[0].settle(
        new ConnectError("down", Code.Unavailable),
      );
    });
    await settle(60);
    expect(pendingInvitation.read()).toBeUndefined();
    expect(window.location.pathname).toBe("/invitations/accept");
  });
});
