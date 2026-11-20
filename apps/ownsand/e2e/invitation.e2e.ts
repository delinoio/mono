// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import {
  MockError,
  ORGANIZATION_ID,
  PENDING_INVITATION_KEY,
  installMockApi,
  organizationHandlers,
  signInBeforeLoad,
} from "./mock-api.ts";

const SECRET = "inv_e2e_secret_123";

function readPending() {
  return (key: string) => window.sessionStorage.getItem(key);
}

describe("invitation acceptance", () => {
  test("keeps the secret across sign-in and accepts it", async ({
    app,
    screen,
    browser,
  }) => {
    const handlers = organizationHandlers("ROLE_MEMBER");
    handlers["MemberService/AcceptInvitation"] = () => ({
      organization: {
        organizationId: ORGANIZATION_ID,
        name: "Acme",
        callerRole: "ROLE_MEMBER",
      },
    });
    const calls = await installMockApi(browser, handlers);

    await app.open(`/invitations/accept#secret=${SECRET}`);

    // The fragment leaves the address bar, and the secret waits in
    // sessionStorage only for the sign-in redirect.
    await expect(screen.getByRole("button", "Sign in")).toBeVisible();
    expect(await browser.url()).not.toContain("#");
    expect(await browser.evaluate(readPending(), PENDING_INVITATION_KEY)).toBe(
      SECRET,
    );

    await screen.getByRole("button", "Sign in").click();

    // The stand-in returns through /callback like Logto does.
    await screen.getByRole("button", "Accept invitation").click();
    await expect(
      screen.getByRole("heading", "Sandboxes", { level: 1 }),
    ).toBeVisible();
    const accept = calls.find(
      (call) => call.procedure === "MemberService/AcceptInvitation",
    );
    expect(accept?.body).toEqual({ invitationSecret: SECRET });
    expect(
      await browser.evaluate(readPending(), PENDING_INVITATION_KEY),
    ).toBeNull();
  });

  test("deletes the secret when acceptance fails", async ({
    app,
    screen,
    browser,
  }) => {
    await signInBeforeLoad(browser);
    const handlers = organizationHandlers("ROLE_MEMBER");
    handlers["MemberService/AcceptInvitation"] = () =>
      new MockError("not_found", 404);
    await installMockApi(browser, handlers);
    await app.open("/");
    await browser.evaluate(
      ([key, value]: [string, string]) => {
        window.sessionStorage.setItem(key, value);
        return null;
      },
      [PENDING_INVITATION_KEY, SECRET],
    );

    await app.open("/invitations/accept");
    await screen.getByRole("button", "Accept invitation").click();

    await expect(screen.getByRole("alert")).toContainText(
      "This invitation is not valid",
    );
    expect(
      await browser.evaluate(readPending(), PENDING_INVITATION_KEY),
    ).toBeNull();
  });

  test("deletes the secret when the user leaves the flow", async ({
    app,
    screen,
    browser,
  }) => {
    await installMockApi(browser, organizationHandlers("ROLE_MEMBER"));
    await app.open(`/invitations/accept#secret=${SECRET}`);
    await expect(screen.getByRole("button", "Sign in")).toBeVisible();

    await screen.getByRole("link", "OwnSand").click();

    await expect(
      screen.getByRole("heading", "Sign in to OwnSand", { level: 3 }),
    ).toBeVisible();
    expect(
      await browser.evaluate(readPending(), PENDING_INVITATION_KEY),
    ).toBeNull();
  });
});
