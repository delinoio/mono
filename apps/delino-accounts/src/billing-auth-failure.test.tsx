// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { ACCOUNT_ID, ADMIN, createFakeApi, OWNER } from "./testing/fake-api.ts";
import { renderApp, settle } from "./testing/render.tsx";

/**
 * The direct billing actions (open an invoice, resume the subscription,
 * change a role, revoke an invitation) offer the sign-in redirect when the
 * session ended, instead of a toast that cannot offer it.
 */

const SESSION_ENDED = "Your session ended. Sign in again.";
const SIGN_IN = { name: "Sign in with Delino" };
const ACCOUNT_PATH = `/billing/${ACCOUNT_ID}`;

const unauthenticated = () =>
  new ConnectError("token expired", Code.Unauthenticated);
/**
 * A failure other than an ended session. The message is unique per action
 * because toasts of earlier tests stay in the document.
 */
const denied = (action: string) =>
  new ConnectError(`Not allowed to ${action}.`, Code.PermissionDenied);

/** Waits for the error toast with `text`. */
async function expectToast(text: string) {
  const titles = await screen.findAllByText(text);
  expect(
    titles.some((el) => el.getAttribute("data-slot") === "toast-title"),
  ).toBe(true);
}

async function click(element: Element) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle(60);
}

async function openTab(name: string) {
  await click(await screen.findByRole("tab", { name }));
}

async function renderAccount(
  userId: string,
  configure: (api: ReturnType<typeof createFakeApi>) => void = () => {},
) {
  const api = createFakeApi();
  api.userId = userId;
  configure(api);
  const result = renderApp(ACCOUNT_PATH, { api });
  await screen.findByRole("heading", { name: "Acme Robotics", level: 1 });
  await settle();
  return result;
}

function calls(api: ReturnType<typeof createFakeApi>, method: string) {
  return api.calls.filter((c) => c.method === method).map((c) => c.request);
}

/** Stands in for the blank tabs that opening an invoice reserves. */
function stubWindowOpen() {
  const original = window.open;
  const tabs: FakeTab[] = [];
  window.open = (() => {
    const tab: FakeTab = {
      opener: "app",
      closed: false,
      closeCalls: 0,
      replaced: [],
      location: {
        replace(url: string) {
          tab.replaced.push(url);
        },
      },
      close() {
        tab.closeCalls += 1;
        tab.closed = true;
      },
    };
    tabs.push(tab);
    return tab;
  }) as unknown as typeof window.open;
  return {
    tabs,
    restore() {
      window.open = original;
    },
  };
}

interface FakeTab {
  opener: unknown;
  closed: boolean;
  closeCalls: number;
  replaced: string[];
  location: { replace(url: string): void };
  close(): void;
}

let restoreWindowOpen: (() => void) | undefined;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  restoreWindowOpen?.();
  restoreWindowOpen = undefined;
});

/** The notice shows the message and the button; no toast repeats it. */
function expectSessionNotice() {
  const notice = document.querySelector(".notice-error");
  expect(notice?.textContent).toContain(SESSION_ENDED);
  expect(
    screen
      .getAllByText(SESSION_ENDED)
      .some((el) => el.getAttribute("data-slot") === "toast-title"),
  ).toBe(false);
  expect(screen.getByRole("button", SIGN_IN)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
}

describe("invoices", () => {
  async function openInvoice(
    failure: ConnectError | undefined,
    stub = stubWindowOpen(),
  ) {
    restoreWindowOpen = stub.restore;
    const rendered = await renderAccount(ADMIN, (api) => {
      if (failure !== undefined) api.failNext.set("GetInvoiceUrl", failure);
    });
    await openTab("Invoices");
    const list = await screen.findByRole("list", { name: "Invoices" });
    await click(within(list).getByRole("button", { name: /Open the invoice/ }));
    return { ...rendered, stub };
  }

  test("an ended session offers sign-in, closes the tab and keeps the list", async () => {
    const { api, signIns, stub } = await openInvoice(unauthenticated());
    expectSessionNotice();
    expect(stub.tabs).toHaveLength(1);
    expect(stub.tabs[0]!.closeCalls).toBe(1);
    expect(stub.tabs[0]!.replaced).toEqual([]);
    const list = screen.getByRole("list", { name: "Invoices" });
    expect(list.textContent).toContain("Top-up · $25.00");
    // Nothing starts or replays by itself.
    expect(signIns).toHaveLength(0);
    expect(calls(api, "GetInvoiceUrl")).toHaveLength(1);
    await click(screen.getByRole("button", SIGN_IN));
    expect(signIns).toHaveLength(1);
    expect(calls(api, "GetInvoiceUrl")).toHaveLength(1);
  });

  test("another failure keeps its toast, closes the tab and shows no sign-in", async () => {
    const { stub } = await openInvoice(denied("open the invoice"));
    await expectToast("Not allowed to open the invoice.");
    expect(stub.tabs).toHaveLength(1);
    expect(stub.tabs[0]!.closeCalls).toBe(1);
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
    expect(document.querySelector(".notice-error")).toBeNull();
  });

  test("a later successful open removes the notice", async () => {
    const { stub } = await openInvoice(unauthenticated());
    expectSessionNotice();
    const list = screen.getByRole("list", { name: "Invoices" });
    await click(within(list).getByRole("button", { name: /Open the invoice/ }));
    expect(stub.tabs).toHaveLength(2);
    expect(stub.tabs[1]!.closeCalls).toBe(0);
    expect(stub.tabs[1]!.replaced).toEqual([
      "https://sandbox-invoice.paddle.test/txn_top1.pdf",
    ]);
    expect(document.querySelector(".notice-error")).toBeNull();
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
  });
});

describe("subscription", () => {
  async function resume(failure: ConnectError | undefined) {
    const rendered = await renderAccount(ADMIN, (api) => {
      api.accounts.get(ACCOUNT_ID)!.subscription.cancelAtPeriodEnd = true;
      if (failure !== undefined)
        api.failNext.set("ResumeSubscription", failure);
    });
    await screen.findByText(/Ends on Dec 1, 2026/);
    await click(screen.getByRole("button", { name: "Resume subscription" }));
    return rendered;
  }

  test("an ended session offers sign-in and keeps the subscription", async () => {
    const { api, signIns } = await resume(unauthenticated());
    expectSessionNotice();
    expect(screen.getByText(/Ends on Dec 1, 2026/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Resume subscription" }),
    ).toBeTruthy();
    expect(signIns).toHaveLength(0);
    expect(calls(api, "ResumeSubscription")).toHaveLength(1);
    await click(screen.getByRole("button", SIGN_IN));
    expect(signIns).toHaveLength(1);
    expect(calls(api, "ResumeSubscription")).toHaveLength(1);
  });

  test("another failure keeps its toast and shows no sign-in", async () => {
    await resume(denied("resume the subscription"));
    await expectToast("Not allowed to resume the subscription.");
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
  });

  test("a successful resume keeps its success and refresh behavior", async () => {
    const { api } = await resume(undefined);
    expect(calls(api, "ResumeSubscription")).toHaveLength(1);
    expect(await screen.findByText(/Renews on Dec 1, 2026/)).toBeTruthy();
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
  });

  test("a retry that succeeds removes the notice", async () => {
    await resume(unauthenticated());
    expectSessionNotice();
    await click(screen.getByRole("button", { name: "Resume subscription" }));
    expect(await screen.findByText(/Renews on Dec 1, 2026/)).toBeTruthy();
    expect(document.querySelector(".notice-error")).toBeNull();
  });
});

describe("members", () => {
  async function changeRole(failure: ConnectError | undefined) {
    const rendered = await renderAccount(OWNER, (api) => {
      if (failure !== undefined) api.failNext.set("UpdateMemberRole", failure);
    });
    await openTab("Members");
    await screen.findByRole("list", { name: "Members" });
    await click(
      await screen.findByRole("combobox", { name: "Role of Vera Viewer" }),
    );
    await click(screen.getByRole("option", { name: "Admin" }));
    return rendered;
  }

  test("an ended session offers sign-in and keeps the member list", async () => {
    const { api, signIns } = await changeRole(unauthenticated());
    expectSessionNotice();
    const list = screen.getByRole("list", { name: "Members" });
    expect(list.textContent).toContain("Vera Viewer");
    expect(signIns).toHaveLength(0);
    expect(calls(api, "UpdateMemberRole")).toHaveLength(1);
    await click(screen.getByRole("button", SIGN_IN));
    expect(signIns).toHaveLength(1);
    expect(calls(api, "UpdateMemberRole")).toHaveLength(1);
  });

  test("another failure keeps its toast and shows no sign-in", async () => {
    await changeRole(denied("change the role"));
    await expectToast("Not allowed to change the role.");
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
  });

  test("a successful change keeps its success and refresh behavior", async () => {
    const { api } = await changeRole(undefined);
    expect(calls(api, "UpdateMemberRole")).toEqual([
      expect.objectContaining({ role: 2 }),
    ]);
    expect(await screen.findByText("The role is changed.")).toBeTruthy();
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
  });
});

describe("invitations", () => {
  async function revoke(failure: ConnectError | undefined) {
    const rendered = await renderAccount(OWNER, (api) => {
      if (failure !== undefined) api.failNext.set("RevokeInvitation", failure);
    });
    await openTab("Members");
    await screen.findByRole("list", { name: "Pending invitations" });
    await click(screen.getByRole("button", { name: "Revoke" }));
    return rendered;
  }

  test("an ended session offers sign-in and keeps the invitation list", async () => {
    const { api, signIns } = await revoke(unauthenticated());
    expectSessionNotice();
    expect(
      screen.getByRole("list", { name: "Pending invitations" }),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy();
    expect(signIns).toHaveLength(0);
    expect(calls(api, "RevokeInvitation")).toHaveLength(1);
    await click(screen.getByRole("button", SIGN_IN));
    expect(signIns).toHaveLength(1);
    expect(calls(api, "RevokeInvitation")).toHaveLength(1);
  });

  test("another failure keeps its toast and shows no sign-in", async () => {
    await revoke(denied("revoke the invitation"));
    await expectToast("Not allowed to revoke the invitation.");
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
  });

  test("a successful revoke keeps its success and refresh behavior", async () => {
    const { api } = await revoke(undefined);
    expect(calls(api, "RevokeInvitation")).toHaveLength(1);
    expect(await screen.findByText("No pending invitations")).toBeTruthy();
    expect(screen.getByText("The invitation is revoked.")).toBeTruthy();
    expect(screen.queryByRole("button", SIGN_IN)).toBeNull();
  });
});
