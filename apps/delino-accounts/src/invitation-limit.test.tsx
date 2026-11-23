// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, expect, test } from "@rstest/core";
import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorMessage, INVITATION_LIMIT_MESSAGE } from "./format.ts";
import { ACCOUNT_ID, createFakeApi } from "./testing/fake-api.ts";
import { renderApp, settle } from "./testing/render.tsx";

/**
 * `CreateInvitation` fails with `resource_exhausted` and the message
 * `too many invitations; try again later` when the inviter or the address
 * reached its limit in 24 hours. The invitation form shows that message.
 */

async function click(element: Element) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle();
}

async function type(element: Element, value: string) {
  await act(async () => {
    fireEvent.input(element, { target: { value } });
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

test("the invitation form shows the limit message and keeps the draft", async () => {
  const api = createFakeApi();
  api.failNext.set(
    "CreateInvitation",
    new ConnectError(INVITATION_LIMIT_MESSAGE, Code.ResourceExhausted),
  );
  renderApp(`/billing/${ACCOUNT_ID}`, { api });
  await screen.findByRole("heading", { name: "Acme Robotics", level: 1 });
  await click(await screen.findByRole("tab", { name: "Members" }));
  await screen.findByRole("list", { name: "Pending invitations" });

  await click(screen.getByRole("button", { name: "Invite by email" }));
  const field = screen.getByRole("textbox", { name: "Email" });
  await type(field, "guest@example.com");
  await click(screen.getByRole("button", { name: "Send invitation" }));

  expect(await screen.findByText(INVITATION_LIMIT_MESSAGE)).toBeTruthy();
  expect(screen.queryByText(/Too many requests/)).toBeNull();
  // The dialog stays open with the address, so the person can try again.
  expect(
    (screen.getByRole("textbox", { name: "Email" }) as HTMLInputElement).value,
  ).toBe("guest@example.com");
  expect(api.calls.filter((c) => c.method === "CreateInvitation")).toHaveLength(
    1,
  );
});

test("other resource_exhausted messages keep the generic text", () => {
  expect(errorMessage(new ConnectError("busy", Code.ResourceExhausted))).toBe(
    "Too many requests. Wait a moment and try again.",
  );
  expect(
    errorMessage(
      new ConnectError(INVITATION_LIMIT_MESSAGE, Code.ResourceExhausted),
    ),
  ).toBe(INVITATION_LIMIT_MESSAGE);
});
