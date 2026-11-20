// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { Role } from "@delino/ownsand-sdk/gen/common_pb";
import { MemberService } from "@delino/ownsand-sdk/gen/organization_pb";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { AuthStatus } from "./auth/auth.tsx";
import {
  INVITATION_PATH,
  PENDING_INVITATION_KEY,
  browserSessionStorage,
  clearPendingSecretOutsideFlow,
} from "./invitation.ts";
import { InvitationPage } from "./pages/invitation-page.tsx";
import { useRouter } from "./router.tsx";
import { TestProviders, fakeAuth } from "./test-support.tsx";

function acceptTransport(
  handler: (secret: string) => { organizationId: string } | ConnectError,
  calls: string[],
) {
  return createRouterTransport(({ service }) => {
    service(MemberService, {
      acceptInvitation: (request) => {
        calls.push(request.invitationSecret);
        const result = handler(request.invitationSecret);
        if (result instanceof ConnectError) {
          throw result;
        }
        return {
          organization: {
            organizationId: result.organizationId,
            name: "Acme",
            callerRole: Role.MEMBER,
          },
        };
      },
    });
  });
}

function clearOutsideFlow(pathname: string) {
  const storage = browserSessionStorage();
  if (storage) {
    clearPendingSecretOutsideFlow(storage, pathname);
  }
}

function NavigateAway() {
  const { navigate } = useRouter();
  return (
    <button type="button" onClick={() => navigate("/")}>
      Leave flow
    </button>
  );
}

beforeEach(() => {
  window.history.replaceState(null, "", "/");
  window.sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
});

describe("InvitationPage", () => {
  test("removes the fragment and keeps the secret only for a signed-out sign-in", async () => {
    window.history.replaceState(null, "", `${INVITATION_PATH}#secret=inv_1`);
    let signIns = 0;
    await act(async () => {
      render(
        <TestProviders
          transport={acceptTransport(() => ({ organizationId: "o1" }), [])}
          auth={fakeAuth({
            status: AuthStatus.SignedOut,
            userId: undefined,
            signIn: () => {
              signIns += 1;
              return Promise.resolve();
            },
          })}
          onPathChange={clearOutsideFlow}
        >
          <InvitationPage />
        </TestProviders>,
      );
    });
    expect(window.location.hash).toBe("");
    expect(window.location.pathname).toBe(INVITATION_PATH);
    expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe("inv_1");

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    });
    expect(signIns).toBe(1);
    expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe("inv_1");
  });

  test("does not store the secret when the user is already signed in", async () => {
    window.history.replaceState(null, "", `${INVITATION_PATH}#secret=inv_2`);
    await act(async () => {
      render(
        <TestProviders
          transport={acceptTransport(() => ({ organizationId: "o1" }), [])}
          auth={fakeAuth()}
        >
          <InvitationPage />
        </TestProviders>,
      );
    });
    expect(window.location.hash).toBe("");
    expect(window.sessionStorage.length).toBe(0);
    expect(
      screen.getByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
  });

  test("accepts the secret kept for the sign-in redirect and then deletes it", async () => {
    window.history.replaceState(null, "", INVITATION_PATH);
    window.sessionStorage.setItem(PENDING_INVITATION_KEY, "inv_3");
    const calls: string[] = [];
    await act(async () => {
      render(
        <TestProviders
          transport={acceptTransport(() => ({ organizationId: "o9" }), calls)}
          auth={fakeAuth()}
          onPathChange={clearOutsideFlow}
        >
          <InvitationPage />
        </TestProviders>,
      );
    });
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Accept invitation" }),
      );
    });
    expect(calls).toEqual(["inv_3"]);
    expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBeNull();
    expect(window.location.pathname).toBe("/orgs/o9/sandboxes");
  });

  test("deletes the secret when AcceptInvitation fails", async () => {
    window.history.replaceState(null, "", INVITATION_PATH);
    window.sessionStorage.setItem(PENDING_INVITATION_KEY, "inv_4");
    await act(async () => {
      render(
        <TestProviders
          transport={acceptTransport(
            () => new ConnectError("gone", Code.NotFound),
            [],
          )}
          auth={fakeAuth()}
        >
          <InvitationPage />
        </TestProviders>,
      );
    });
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Accept invitation" }),
      );
    });
    expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain(
      "This invitation is not valid",
    );
    // The raw server message is never shown.
    expect(document.body.textContent).not.toContain("gone");
  });

  test("deletes the secret when the user navigates away from the flow", async () => {
    window.history.replaceState(null, "", INVITATION_PATH);
    window.sessionStorage.setItem(PENDING_INVITATION_KEY, "inv_5");
    await act(async () => {
      render(
        <TestProviders
          transport={acceptTransport(() => ({ organizationId: "o1" }), [])}
          auth={fakeAuth({ status: AuthStatus.SignedOut, userId: undefined })}
          onPathChange={clearOutsideFlow}
        >
          <InvitationPage />
          <NavigateAway />
        </TestProviders>,
      );
    });
    expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe("inv_5");
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Leave flow" }));
    });
    expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBeNull();
  });
});
