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
import { BillingAccountRole } from "@delino/accounts-protos/v1/billing_account_pb";
import { SubscriptionStatus } from "@delino/accounts-protos/v1/credit_pb";
import { pendingInvitation } from "./invitation.ts";
import {
  ACCOUNT_ID,
  ADMIN,
  addBillingAccounts,
  addInvitations,
  addLinkedOrganizations,
  addMembers,
  createFakeApi,
  INVITATION_TOKEN,
  numbered,
  OUTSIDER,
  VIEWER,
} from "./testing/fake-api.ts";
import { fakeCheckout, renderApp, settle } from "./testing/render.tsx";

/** A token of an earlier link that waits in `sessionStorage`. */
const EARLIER_INVITATION_TOKEN = "bai_earlier-token";
const ACCOUNT_PATH = `/billing/${ACCOUNT_ID}`;
const UUID_V7 =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

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

async function openTab(name: string) {
  await click(await screen.findByRole("tab", { name }));
}

/** Renders the account page as `userId` and waits for it. */
async function renderAccount(
  userId: string | undefined = undefined,
  options: Parameters<typeof renderApp>[1] = {},
) {
  const api = options.api ?? createFakeApi();
  if (userId !== undefined) api.userId = userId;
  const result = renderApp(ACCOUNT_PATH, { ...options, api });
  await screen.findByRole("heading", { name: "Acme Robotics", level: 1 });
  await settle();
  return result;
}

function calls(api: ReturnType<typeof createFakeApi>, method: string) {
  return api.calls.filter((c) => c.method === method).map((c) => c.request);
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

describe("sign-in", () => {
  test("billing pages ask a signed-out user to sign in", async () => {
    const { signIns } = renderApp("/billing", { signedIn: false });
    await click(
      await screen.findByRole("button", { name: "Sign in with Delino" }),
    );
    expect(signIns).toHaveLength(1);
  });

  test("a build without Logto shows that sign-in is not configured", async () => {
    renderApp("/billing", {
      signedIn: false,
      config: {
        apiOrigin: "https://accounts.api.test",
        logto: undefined,
        paddle: undefined,
      },
    });
    expect(
      await screen.findByText("Sign-in is not configured for this build."),
    ).toBeTruthy();
  });

  test("the home page links to billing without sign-in", async () => {
    renderApp("/", { signedIn: false });
    expect(
      await screen.findByRole("heading", { name: "Delino Accounts", level: 1 }),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Manage billing" }).getAttribute("href"),
    ).toBe("/billing");
    expect(screen.getByText("Coming soon")).toBeTruthy();
  });

  test("an unknown path shows the not-found page", async () => {
    renderApp("/billing/a/b");
    expect(
      await screen.findByRole("heading", { name: "Page not found" }),
    ).toBeTruthy();
  });
});

describe("ended session", () => {
  const SESSION_ENDED = "Your session ended. Sign in again.";

  test("an Unauthenticated answer offers the sign-in redirect and no retry", async () => {
    const api = createFakeApi();
    api.failNext.set(
      "ListBillingAccounts",
      new ConnectError("token expired", Code.Unauthenticated),
    );
    const { signIns } = renderApp("/billing", { api });
    expect(await screen.findByText(SESSION_ENDED)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(signIns).toHaveLength(0);
    await click(screen.getByRole("button", { name: "Sign in with Delino" }));
    expect(signIns).toHaveLength(1);
  });

  test("a token source without a token offers the sign-in redirect", async () => {
    // The Logto SDK keeps isAuthenticated true after a failed refresh, and
    // the app's own transport then throws Unauthenticated before any request.
    const { signIns } = renderApp("/billing", {
      appTransport: true,
      getAccessToken: async () => undefined,
    });
    expect(await screen.findByText(SESSION_ENDED)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await click(screen.getByRole("button", { name: "Sign in with Delino" }));
    expect(signIns).toHaveLength(1);
  });

  test("an Unavailable answer keeps the retry button and offers no sign-in", async () => {
    const api = createFakeApi();
    api.failNext.set(
      "ListBillingAccounts",
      new ConnectError("down", Code.Unavailable),
    );
    const { signIns } = renderApp("/billing", { api });
    expect(
      await screen.findByText(/Delino Accounts is not available/),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in with Delino" })).toBe(
      null,
    );
    await click(screen.getByRole("button", { name: "Try again" }));
    expect(
      await screen.findByRole("link", { name: /Acme Robotics/ }),
    ).toBeTruthy();
    expect(signIns).toHaveLength(0);
  });
});

describe("billing accounts", () => {
  test("lists the caller's billing accounts with the caller's role", async () => {
    renderApp("/billing");
    const link = await screen.findByRole("link", { name: /Acme Robotics/ });
    expect(link.textContent).toContain("Owner");
    expect(link.getAttribute("href")).toBe(ACCOUNT_PATH);
  });

  test("an outsider sees the empty state", async () => {
    const api = createFakeApi();
    api.userId = OUTSIDER;
    renderApp("/billing", { api });
    expect(await screen.findByText("No billing accounts yet")).toBeTruthy();
  });

  test("creates a billing account: focuses the name, checks it and opens the new account", async () => {
    const { api } = renderApp("/billing");
    await screen.findByRole("link", { name: /Acme Robotics/ });
    await click(screen.getByRole("button", { name: "Create billing account" }));
    await settle(60);
    const input = screen.getByRole("textbox", { name: "Name" });
    expect(document.activeElement).toBe(input);
    await type(input, "   ");
    await click(screen.getByRole("button", { name: "Create" }));
    expect(screen.getByText("Enter a name.")).toBeTruthy();
    await type(input, "x".repeat(101));
    await click(screen.getByRole("button", { name: "Create" }));
    expect(screen.getByText("Use at most 100 characters.")).toBeTruthy();
    expect(calls(api, "CreateBillingAccount")).toHaveLength(0);
    await type(input, "  Northwind  ");
    await click(screen.getByRole("button", { name: "Create" }));
    await settle(60);
    expect(calls(api, "CreateBillingAccount")).toEqual([
      expect.objectContaining({ name: "Northwind" }),
    ]);
    expect(
      await screen.findByRole("heading", { name: "Northwind", level: 1 }),
    ).toBeTruthy();
  });

  test("the create dialog closes with Escape", async () => {
    renderApp("/billing");
    await screen.findByRole("link", { name: /Acme Robotics/ });
    await click(screen.getByRole("button", { name: "Create billing account" }));
    const dialog = await screen.findByRole("dialog");
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("only the owner can delete; a failure shows in the dialog", async () => {
    const { api } = await renderAccount();
    await openTab("Settings");
    await click(screen.getByRole("button", { name: "Delete billing account" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(document.activeElement?.textContent).toBe("Cancel");
    await click(
      within(dialog).getByRole("button", { name: "Delete billing account" }),
    );
    await settle(60);
    expect(within(dialog).getByRole("alert").textContent).toContain(
      "linked organizations",
    );
    api.accounts.get(ACCOUNT_ID)!.linked = [];
    await click(
      within(dialog).getByRole("button", { name: "Delete billing account" }),
    );
    await settle(60);
    expect(window.location.pathname).toBe("/billing");
    expect(await screen.findByText("No billing accounts yet")).toBeTruthy();
  });

  test("an admin does not see the delete action", async () => {
    await renderAccount(ADMIN);
    await openTab("Settings");
    await screen.findByRole("heading", { name: "Automatic top-up" });
    expect(
      screen.queryByRole("button", { name: "Delete billing account" }),
    ).toBeNull();
  });

  test("renames the billing account", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    const name = screen.getByRole("textbox", { name: "Billing account name" });
    await type(name, "Acme Labs");
    const form = name.closest("form")!;
    await click(within(form).getByRole("button", { name: "Save" }));
    await settle(60);
    expect(calls(api, "UpdateBillingAccount")).toEqual([
      expect.objectContaining({ name: "Acme Labs" }),
    ]);
  });
});

async function balanceText() {
  const region = await screen.findByRole("region", { name: "Balance" });
  return region.querySelector(".balance")?.textContent;
}

describe("overview", () => {
  test("shows the balance, the credit grants and their expiry", async () => {
    await renderAccount();
    expect(await balanceText()).toBe("$12.35");
    const grants = await screen.findByRole("list", { name: "Credit grants" });
    expect(grants.textContent).toContain("$2.35 of $10.00");
    expect(grants.textContent).toContain("Expires Nov 1, 2027");
    expect(grants.textContent).toContain("$10.00 of $25.00");
  });

  test("shows a negative balance as debt", async () => {
    const api = createFakeApi();
    api.accounts.get(ACCOUNT_ID)!.balance = -1_500_000n;
    await renderAccount(undefined, { api });
    expect(await balanceText()).toBe("-$1.50");
    expect(screen.getByText(/The balance is negative/)).toBeTruthy();
  });
});

describe("subscription", () => {
  test("starts the Paddle checkout with the transaction from StartSubscription and shows the pending state", async () => {
    const api = createFakeApi();
    api.accounts.get(ACCOUNT_ID)!.subscription = {
      status: SubscriptionStatus.NONE,
      cancelAtPeriodEnd: false,
    };
    const checkout = fakeCheckout();
    await renderAccount(undefined, { api, checkout });
    await click(
      await screen.findByRole("button", { name: "Start subscription" }),
    );
    await settle(60);
    expect(checkout.opened).toEqual(["txn_checkout1"]);
    await act(async () => checkout.close());
    await settle(60);
    expect(screen.getByText("Waiting for payment")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Continue checkout" }),
    ).toBeTruthy();
    // Top-ups need an active subscription.
    expect(
      (screen.getByRole("button", { name: "Top up" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  test("shows that checkout is not configured without Paddle settings", async () => {
    const api = createFakeApi();
    api.accounts.get(ACCOUNT_ID)!.subscription = {
      status: SubscriptionStatus.CANCELED,
      cancelAtPeriodEnd: false,
    };
    await renderAccount(undefined, { api });
    expect(
      await screen.findByText("Checkout is not configured for this build."),
    ).toBeTruthy();
  });

  test("cancels at the period end and resumes", async () => {
    const { api } = await renderAccount(ADMIN);
    expect(await screen.findByText(/Renews on Dec 1, 2026/)).toBeTruthy();
    await click(screen.getByRole("button", { name: "Cancel subscription" }));
    const dialog = await screen.findByRole("alertdialog");
    await click(
      within(dialog).getByRole("button", { name: "Cancel subscription" }),
    );
    await settle(60);
    expect(calls(api, "CancelSubscription")).toHaveLength(1);
    expect(await screen.findByText(/Ends on Dec 1, 2026/)).toBeTruthy();
    await click(screen.getByRole("button", { name: "Resume subscription" }));
    await settle(60);
    expect(calls(api, "ResumeSubscription")).toHaveLength(1);
    expect(await screen.findByText(/Renews on Dec 1, 2026/)).toBeTruthy();
  });

  test("a viewer sees the subscription without actions", async () => {
    await renderAccount(VIEWER);
    expect(await screen.findByText(/Renews on/)).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Cancel subscription" }),
    ).toBeNull();
    expect(screen.queryByRole("button", { name: "Top up" })).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Start subscription" }),
    ).toBeNull();
  });
});

describe("top-up", () => {
  async function openTopUp() {
    await click(await screen.findByRole("button", { name: "Top up" }));
    await settle(60);
    return screen.getByRole("textbox", { name: "Amount (USD)" });
  }

  test("focuses the amount and checks the minimum, maximum and whole cents before sending", async () => {
    const { api } = await renderAccount();
    const input = await openTopUp();
    expect(document.activeElement).toBe(input);
    for (const [value, message] of [
      ["24.99", "Enter at least $25.00."],
      ["10000.01", "Enter at most $10,000.00."],
      ["25.001", "Use whole cents (at most 2 decimal places)."],
      ["abc", "Enter an amount in USD, for example 25.00."],
    ] as const) {
      await type(input, value);
      await click(
        within(screen.getByRole("dialog")).getByRole("button", {
          name: "Top up",
        }),
      );
      expect(screen.getByText(message)).toBeTruthy();
    }
    expect(calls(api, "CreateTopUp")).toHaveLength(0);
  });

  test("sends bigint micro-USD and reuses the idempotency key for retries of one top-up", async () => {
    const { api } = await renderAccount();
    const input = await openTopUp();
    await type(input, "25.50");
    api.failNext.set(
      "CreateTopUp",
      new ConnectError("Paddle is unavailable", Code.Unavailable),
    );
    const dialog = screen.getByRole("dialog");
    await click(within(dialog).getByRole("button", { name: "Top up" }));
    await settle(60);
    expect(within(dialog).getByRole("alert")).toBeTruthy();
    await click(within(dialog).getByRole("button", { name: "Top up" }));
    await settle(60);
    const requests = calls(api, "CreateTopUp");
    expect(requests).toHaveLength(2);
    expect(requests[0]!.amountUsdMicros).toBe(25_500_000n);
    expect(requests[0]!.idempotencyKey).toMatch(UUID_V7);
    expect(requests[1]!.idempotencyKey).toBe(requests[0]!.idempotencyKey);
    expect(screen.queryByRole("dialog")).toBeNull();
    // A new top-up gets a new key.
    await openTopUp();
    await click(
      within(screen.getByRole("dialog")).getByRole("button", {
        name: "Top up",
      }),
    );
    await settle(60);
    const third = calls(api, "CreateTopUp")[2]!;
    expect(third.amountUsdMicros).toBe(25_000_000n);
    expect(third.idempotencyKey).not.toBe(requests[0]!.idempotencyKey);
  });
});

describe("usage", () => {
  test("shows the monthly summary with sub-cent costs and the ledger", async () => {
    const { api } = await renderAccount(VIEWER);
    await openTab("Usage");
    expect((await screen.findByLabelText("Usage total")).textContent).toBe(
      "$1.25",
    );
    const table = screen.getByRole("table");
    expect(table.textContent).toContain("Relay egress");
    expect(table.textContent).toContain("1,048,576 bytes");
    expect(table.textContent).toContain("$0.004567");
    expect(table.textContent).toContain("12,000 calls");
    const ledger = screen.getByRole("list", { name: "Ledger entries" });
    expect(ledger.textContent).toContain("-$0.000123");
    expect(ledger.textContent).toContain("OwnSand · org-1 · Relay egress");
    expect(ledger.textContent).toContain("+$10.00");
    // An earlier month.
    expect(
      (screen.getByRole("button", { name: "Next month" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    await click(screen.getByRole("button", { name: "Previous month" }));
    await settle(60);
    expect(await screen.findByText("No usage in this month")).toBeTruthy();
    const months = calls(api, "ListUsageSummary");
    const now = new Date();
    const previous = new Date(
      Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - 1, 1),
    );
    expect(months.at(-1)).toEqual(
      expect.objectContaining({
        year: previous.getUTCFullYear(),
        month: previous.getUTCMonth() + 1,
      }),
    );
  });
});

describe("organizations", () => {
  test("lists linked organizations read-only", async () => {
    await renderAccount(VIEWER);
    await openTab("Organizations");
    const list = await screen.findByRole("list", {
      name: "Linked organizations",
    });
    expect(list.textContent).toContain("Acme sandboxes");
    expect(list.textContent).toContain("OwnSand");
    expect(list.textContent).toContain("Active");
    expect(within(list).queryByRole("button")).toBeNull();
  });
});

describe("invoices", () => {
  test("a viewer has no invoices tab", async () => {
    await renderAccount(VIEWER);
    expect(screen.queryByRole("tab", { name: "Invoices" })).toBeNull();
  });

  test("opens the invoice link that GetInvoiceUrl returns without storing it", async () => {
    const opened: { replaced?: string; opener: unknown; closed: boolean } = {
      opener: "app",
      closed: false,
    };
    const originalOpen = window.open;
    window.open = (() => ({
      get opener() {
        return opened.opener;
      },
      set opener(value: unknown) {
        opened.opener = value;
      },
      closed: false,
      location: {
        replace(url: string) {
          opened.replaced = url;
        },
      },
      close() {
        opened.closed = true;
      },
    })) as unknown as typeof window.open;
    try {
      const { api } = await renderAccount(ADMIN);
      await openTab("Invoices");
      const list = await screen.findByRole("list", { name: "Invoices" });
      expect(list.textContent).toContain("Top-up · $25.00");
      await click(
        within(list).getByRole("button", { name: /Open the invoice/ }),
      );
      await settle(60);
      expect(calls(api, "GetInvoiceUrl")).toEqual([
        expect.objectContaining({ paddleTransactionId: "txn_top1" }),
      ]);
      expect(opened.replaced).toBe(
        "https://sandbox-invoice.paddle.test/txn_top1.pdf",
      );
      expect(opened.opener).toBeNull();
      const stored = JSON.stringify({
        ...window.sessionStorage,
        ...window.localStorage,
      });
      expect(stored).not.toContain("sandbox-invoice");
    } finally {
      window.open = originalOpen;
    }
  });
});

describe("members", () => {
  test("the owner manages admins and viewers but not the owner row", async () => {
    await renderAccount();
    await openTab("Members");
    const list = await screen.findByRole("list", { name: "Members" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows[0]!.textContent).toContain("Olivia Owner");
    expect(within(rows[0]!).queryByRole("button")).toBeNull();
    expect(
      within(rows[1]!).getByRole("button", { name: "Make owner" }),
    ).toBeTruthy();
    expect(
      within(rows[2]!).getByRole("button", { name: "Remove" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("combobox", { name: "Role of Vera Viewer" }),
    ).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "Leave billing account" }),
    ).toBeNull();
  });

  test("an admin cannot transfer ownership but can leave", async () => {
    await renderAccount(ADMIN);
    await openTab("Members");
    await screen.findByRole("list", { name: "Members" });
    expect(screen.queryByRole("button", { name: "Make owner" })).toBeNull();
    expect(
      screen.getByRole("button", { name: "Leave billing account" }),
    ).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Invitations" })).toBeTruthy();
  });

  test("a viewer sees members read-only and can leave", async () => {
    const { api } = await renderAccount(VIEWER);
    await openTab("Members");
    const list = await screen.findByRole("list", { name: "Members" });
    expect(within(list).queryByRole("button")).toBeNull();
    expect(within(list).queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Invitations" })).toBeNull();
    expect(calls(api, "ListInvitations")).toHaveLength(0);
    await click(screen.getByRole("button", { name: "Leave billing account" }));
    const dialog = await screen.findByRole("alertdialog");
    await click(
      within(dialog).getByRole("button", { name: "Leave billing account" }),
    );
    await settle(60);
    expect(calls(api, "RemoveMember")).toEqual([
      expect.objectContaining({ userId: VIEWER }),
    ]);
    expect(window.location.pathname).toBe("/billing");
  });

  test.each([
    ["a viewer", VIEWER],
    ["an admin", ADMIN],
  ])(
    "%s who leaves never sees the not-found notice and the account is gone from the list",
    async (_role, userId) => {
      const { api } = await renderAccount(userId);
      await openTab("Members");
      await screen.findByRole("list", { name: "Members" });
      // Record the not-found text at every DOM change and at the navigation
      // itself, so that a flash between the removal and the navigation counts.
      const notFound = "It was not found, or you no longer have access to it.";
      const seen: string[] = [];
      const record = (where: string) => {
        if (document.body.textContent?.includes(notFound)) seen.push(where);
      };
      const observer = new MutationObserver(() => record("mutation"));
      observer.observe(document.body, {
        childList: true,
        subtree: true,
        characterData: true,
      });
      const pushState = window.history.pushState.bind(window.history);
      window.history.pushState = (...args) => {
        record("navigation");
        pushState(...args);
      };
      // Hold the account request that a refresh would send. The page must not
      // wait for it: the user cannot read the account any more, so the request
      // can only fail with the not-found notice.
      let release!: () => void;
      api.gates.set(
        "GetBillingAccount",
        new Promise<void>((resolve) => {
          release = resolve;
        }),
      );
      try {
        await click(
          screen.getByRole("button", { name: "Leave billing account" }),
        );
        const dialog = await screen.findByRole("alertdialog");
        await click(
          within(dialog).getByRole("button", { name: "Leave billing account" }),
        );
        await settle(100);
        expect(calls(api, "RemoveMember")).toHaveLength(1);
        expect(window.location.pathname).toBe("/billing");
        release();
        await settle(100);
        expect(await screen.findByText("No billing accounts yet")).toBeTruthy();
        expect(
          screen.queryByRole("link", { name: /Acme Robotics/ }),
        ).toBeNull();
        expect(seen).toEqual([]);
      } finally {
        observer.disconnect();
        window.history.pushState = pushState;
      }
      expect(calls(api, "RemoveMember")).toEqual([
        expect.objectContaining({ userId }),
      ]);
    },
  );

  test("a failed leave shows its error in the dialog and does not navigate", async () => {
    const { api } = await renderAccount(VIEWER);
    await openTab("Members");
    await screen.findByRole("list", { name: "Members" });
    api.failNext.set(
      "RemoveMember",
      new ConnectError("the owner cannot leave", Code.FailedPrecondition),
    );
    await click(screen.getByRole("button", { name: "Leave billing account" }));
    const dialog = await screen.findByRole("alertdialog");
    await click(
      within(dialog).getByRole("button", { name: "Leave billing account" }),
    );
    await settle(60);
    expect(within(dialog).getByRole("alert")).toBeTruthy();
    expect(window.location.pathname).toBe(ACCOUNT_PATH);
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(
      screen.getByRole("heading", { name: "Acme Robotics", level: 1 }),
    ).toBeTruthy();
  });

  test("changes a role between admin and viewer", async () => {
    const { api } = await renderAccount();
    await openTab("Members");
    const listed = calls(api, "ListMembers").length;
    await click(
      await screen.findByRole("combobox", { name: "Role of Vera Viewer" }),
    );
    await click(screen.getByRole("option", { name: "Admin" }));
    await settle(60);
    expect(calls(api, "UpdateMemberRole")).toEqual([
      expect.objectContaining({ userId: VIEWER, role: 2 }),
    ]);
    expect(calls(api, "ListMembers").length).toBeGreaterThan(listed);
  });

  test("removes a member and transfers ownership after confirmation", async () => {
    const { api } = await renderAccount();
    await openTab("Members");
    const list = await screen.findByRole("list", { name: "Members" });
    const listed = () => calls(api, "ListMembers").length;
    let before = listed();
    await click(within(list).getAllByRole("button", { name: "Remove" })[1]!);
    let dialog = await screen.findByRole("alertdialog");
    await click(within(dialog).getByRole("button", { name: "Remove member" }));
    await settle(60);
    expect(calls(api, "RemoveMember")).toEqual([
      expect.objectContaining({ userId: VIEWER }),
    ]);
    // The member list is refreshed and no longer shows the removed member.
    expect(listed()).toBeGreaterThan(before);
    expect(
      within(await screen.findByRole("list", { name: "Members" })).queryByText(
        /Vera Viewer/,
      ),
    ).toBeNull();
    before = listed();
    await click(screen.getByRole("button", { name: "Make owner" }));
    dialog = await screen.findByRole("alertdialog");
    await click(
      within(dialog).getByRole("button", { name: "Transfer ownership" }),
    );
    await settle(60);
    expect(calls(api, "TransferOwnership")).toEqual([
      expect.objectContaining({ userId: ADMIN }),
    ]);
    expect(listed()).toBeGreaterThan(before);
  });

  test("invites by email: focuses the email, checks it and sends the role", async () => {
    const { api } = await renderAccount();
    await openTab("Members");
    const pending = await screen.findByRole("list", {
      name: "Pending invitations",
    });
    expect(pending.textContent).toContain("pending@example.com");
    await click(screen.getByRole("button", { name: "Invite by email" }));
    await settle(60);
    const email = screen.getByRole("textbox", { name: "Email" });
    expect(document.activeElement).toBe(email);
    await type(email, "not-an-email");
    await click(screen.getByRole("button", { name: "Send invitation" }));
    expect(
      screen.getByText("Enter an email address like name@example.com."),
    ).toBeTruthy();
    expect(calls(api, "CreateInvitation")).toHaveLength(0);
    await type(email, " new@example.com ");
    await click(screen.getByRole("button", { name: "Send invitation" }));
    await settle(60);
    expect(calls(api, "CreateInvitation")).toEqual([
      expect.objectContaining({ email: "new@example.com", role: 3 }),
    ]);
    expect(
      (await screen.findByRole("list", { name: "Pending invitations" }))
        .textContent,
    ).toContain("new@example.com");
  });

  test("revokes a pending invitation", async () => {
    const { api } = await renderAccount();
    await openTab("Members");
    await screen.findByRole("list", { name: "Pending invitations" });
    await click(screen.getByRole("button", { name: "Revoke" }));
    await settle(60);
    expect(calls(api, "RevokeInvitation")).toHaveLength(1);
    expect(await screen.findByText("No pending invitations")).toBeTruthy();
  });
});

describe("invitation acceptance", () => {
  test("removes the token from the address and keeps it only for the sign-in redirect", async () => {
    const { signIns } = renderApp(
      `/billing/invitations/accept#token=${INVITATION_TOKEN}`,
      { signedIn: false },
    );
    expect(window.location.hash).toBe("");
    expect(pendingInvitation.read()).toBeUndefined();
    await click(
      await screen.findByRole("button", { name: "Sign in to accept" }),
    );
    expect(signIns).toHaveLength(1);
    expect(pendingInvitation.read()).toBe(INVITATION_TOKEN);
  });

  test("accepts the pending token after sign-in, deletes it and opens the account", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    const api = createFakeApi();
    api.userId = OUTSIDER;
    renderApp("/billing/invitations/accept", { api });
    await click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
    await settle(60);
    expect(calls(api, "AcceptInvitation")).toEqual([
      expect.objectContaining({ invitationToken: INVITATION_TOKEN }),
    ]);
    expect(pendingInvitation.read()).toBeUndefined();
    expect(window.location.pathname).toBe(ACCOUNT_PATH);
    expect(
      await screen.findByRole("heading", { name: "Acme Robotics", level: 1 }),
    ).toBeTruthy();
  });

  test("a failure shows the error and still deletes the stored token", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    renderApp("/billing/invitations/accept");
    await click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
    await settle(60);
    expect(await screen.findByText(/already a member/)).toBeTruthy();
    expect(pendingInvitation.read()).toBeUndefined();
  });

  test("an ended session offers sign-in and keeps the token for the redirect", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    const api = createFakeApi();
    api.userId = OUTSIDER;
    api.failNext.set(
      "AcceptInvitation",
      new ConnectError("token expired", Code.Unauthenticated),
    );
    const { signIns } = renderApp("/billing/invitations/accept", { api });
    await click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
    await settle(60);
    // The failed attempt deleted the stored token.
    expect(pendingInvitation.read()).toBeUndefined();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    await click(screen.getByRole("button", { name: "Sign in with Delino" }));
    expect(signIns).toHaveLength(1);
    expect(pendingInvitation.read()).toBe(INVITATION_TOKEN);
  });

  test("a new link drops the stored token of an earlier link", async () => {
    pendingInvitation.save(EARLIER_INVITATION_TOKEN);
    renderApp(`/billing/invitations/accept#token=${INVITATION_TOKEN}`);
    expect(
      await screen.findByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
    expect(window.location.hash).toBe("");
    expect(pendingInvitation.read()).toBeUndefined();
    cleanup();
    renderApp("/billing/invitations/accept");
    expect(await screen.findByText(/needs the link/)).toBeTruthy();
  });

  test("accepts the new link and not the stored token of an earlier link", async () => {
    const api = createFakeApi();
    api.userId = OUTSIDER;
    api.invitationTokens.set(EARLIER_INVITATION_TOKEN, {
      accountId: ACCOUNT_ID,
      role: BillingAccountRole.ADMIN,
    });
    pendingInvitation.save(EARLIER_INVITATION_TOKEN);
    renderApp(`/billing/invitations/accept#token=${INVITATION_TOKEN}`, { api });
    await click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
    await settle(60);
    expect(calls(api, "AcceptInvitation")).toEqual([
      expect.objectContaining({ invitationToken: INVITATION_TOKEN }),
    ]);
  });

  test("a link opened while the page is shown drops the stored token of an earlier link", async () => {
    pendingInvitation.save(EARLIER_INVITATION_TOKEN);
    renderApp("/billing/invitations/accept");
    expect(
      await screen.findByRole("button", { name: "Accept invitation" }),
    ).toBeTruthy();
    await act(async () => {
      window.location.hash = `token=${INVITATION_TOKEN}`;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await settle(30);
    expect(pendingInvitation.read()).toBeUndefined();
    expect(window.location.hash).toBe("");
    cleanup();
    renderApp("/billing/invitations/accept");
    expect(await screen.findByText(/needs the link/)).toBeTruthy();
  });

  test("keeps the stored token for the sign-in return when the fragment is empty", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    renderApp("/billing/invitations/accept", { signedIn: false });
    expect(
      await screen.findByRole("button", { name: "Sign in to accept" }),
    ).toBeTruthy();
    expect(pendingInvitation.read()).toBe(INVITATION_TOKEN);
  });

  test("leaving the invitation flow deletes the stored token", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    renderApp("/billing/invitations/accept");
    await click(await screen.findByRole("link", { name: "Not now" }));
    expect(pendingInvitation.read()).toBeUndefined();
  });
});

describe("settings", () => {
  test("checks the automatic top-up minimums and the required cap before saving", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    await click(
      await screen.findByRole("switch", { name: "Automatic top-up" }),
    );
    const threshold = screen.getByRole("textbox", { name: "Threshold (USD)" });
    const amount = screen.getByRole("textbox", { name: "Top-up amount (USD)" });
    const cap = screen.getByRole("textbox", { name: "Monthly cap (USD)" });
    const form = threshold.closest("form")!;
    const save = within(form).getByRole("button", { name: "Save" });
    await type(threshold, "9.99");
    await type(amount, "24.99");
    await click(save);
    expect(screen.getByText("Enter at least $10.00.")).toBeTruthy();
    expect(screen.getByText("Enter at least $25.00.")).toBeTruthy();
    expect(screen.getByText("Enter an amount.")).toBeTruthy();
    await type(threshold, "10");
    await type(amount, "50");
    await type(cap, "40");
    await click(save);
    expect(
      screen.getByText("Enter at least the top-up amount ($50.00)."),
    ).toBeTruthy();
    expect(calls(api, "UpdateAutoTopUpSettings")).toHaveLength(0);
    await type(cap, "200.00");
    await click(save);
    await settle(60);
    expect(calls(api, "UpdateAutoTopUpSettings")).toEqual([
      expect.objectContaining({
        settings: expect.objectContaining({
          enabled: true,
          thresholdUsdMicros: 10_000_000n,
          amountUsdMicros: 50_000_000n,
          monthlyCapUsdMicros: 200_000_000n,
        }),
      }),
    ]);
  });

  test("rejects an enabled automatic top-up amount above $10,000 without a request", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    await click(
      await screen.findByRole("switch", { name: "Automatic top-up" }),
    );
    const threshold = screen.getByRole("textbox", { name: "Threshold (USD)" });
    const amount = screen.getByRole("textbox", { name: "Top-up amount (USD)" });
    const cap = screen.getByRole("textbox", { name: "Monthly cap (USD)" });
    const form = threshold.closest("form")!;
    const save = within(form).getByRole("button", { name: "Save" });
    await type(threshold, "10");
    await type(amount, "15000");
    await type(cap, "20000");
    await click(save);
    expect(screen.getByText("Enter at most $10,000.00.")).toBeTruthy();
    expect(calls(api, "UpdateAutoTopUpSettings")).toHaveLength(0);
    await type(amount, "10000");
    await click(save);
    await settle(60);
    expect(calls(api, "UpdateAutoTopUpSettings")).toEqual([
      expect.objectContaining({
        settings: expect.objectContaining({
          enabled: true,
          amountUsdMicros: 10_000_000_000n,
          monthlyCapUsdMicros: 20_000_000_000n,
        }),
      }),
    ]);
  });

  test("saves the low-balance alert threshold from $0 with the $10 default", async () => {
    const api = createFakeApi();
    await renderAccount(undefined, { api });
    await openTab("Settings");
    const input = await screen.findByRole("textbox", {
      name: "Alert threshold (USD)",
    });
    expect((input as HTMLInputElement).value).toBe("10.00");
    const save = within(input.closest("form")!).getByRole("button", {
      name: "Save",
    });
    await type(input, "-1");
    await click(save);
    expect(
      screen.getByText("Enter an amount in USD, for example 25.00."),
    ).toBeTruthy();
    await type(input, "0");
    await click(save);
    await settle(60);
    expect(calls(api, "UpdateAlertSettings")).toEqual([
      expect.objectContaining({
        settings: expect.objectContaining({
          lowBalanceThresholdUsdMicros: 0n,
        }),
      }),
    ]);
  });

  test("a viewer sees the settings read-only", async () => {
    await renderAccount(VIEWER);
    await openTab("Settings");
    const threshold = await screen.findByRole("textbox", {
      name: "Threshold (USD)",
    });
    expect((threshold as HTMLInputElement).disabled).toBe(true);
    expect(
      (
        screen.getByRole("textbox", {
          name: "Alert threshold (USD)",
        }) as HTMLInputElement
      ).disabled,
    ).toBe(true);
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(
      screen.getAllByText("Only owners and admins can change these settings.")
        .length,
    ).toBe(3);
  });
});

/** The titles of the rows of a list, in order. */
function rowTitles(list: HTMLElement) {
  return within(list)
    .getAllByRole("listitem")
    .map((row) => row.querySelector(".list-title")?.textContent ?? "");
}

/** The continuation tokens that the requests of a method sent, in order. */
function pageTokens(api: ReturnType<typeof createFakeApi>, method: string) {
  return calls(api, method).map((request) => request.pageToken);
}

describe("paged lists", () => {
  describe("billing accounts", () => {
    test("reads every page in server order", async () => {
      const api = createFakeApi();
      addBillingAccounts(api, 200);
      renderApp("/billing", { api });
      const list = await screen.findByRole("list", {
        name: "Billing accounts",
      });
      await settle(100);
      const titles = rowTitles(list);
      expect(titles).toHaveLength(201);
      expect(titles[0]).toBe("Acme Robotics");
      expect(titles[1]).toBe(`Account ${numbered(1)}`);
      expect(titles[200]).toBe(`Account ${numbered(200)}`);
      expect(pageTokens(api, "ListBillingAccounts")).toEqual(["", "p1"]);
      expect(calls(api, "ListBillingAccounts")[1]).toMatchObject({
        pageSize: 200,
      });
    });

    test("an exact page and an empty token make no continuation request", async () => {
      const api = createFakeApi();
      addBillingAccounts(api, 199);
      renderApp("/billing", { api });
      const list = await screen.findByRole("list", {
        name: "Billing accounts",
      });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      expect(pageTokens(api, "ListBillingAccounts")).toEqual([""]);
    });

    test("no account keeps the empty state and makes one request", async () => {
      const api = createFakeApi();
      api.userId = OUTSIDER;
      renderApp("/billing", { api });
      expect(await screen.findByText("No billing accounts yet")).toBeTruthy();
      await settle(100);
      expect(pageTokens(api, "ListBillingAccounts")).toEqual([""]);
    });

    test("a failed continuation keeps the first page and retries at the failed page", async () => {
      const api = createFakeApi();
      addBillingAccounts(api, 200);
      api.failContinuation.set(
        "ListBillingAccounts",
        new ConnectError("temporarily unavailable", Code.Unavailable),
      );
      renderApp("/billing", { api });
      const list = await screen.findByRole("list", {
        name: "Billing accounts",
      });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      expect(await screen.findByText(/Some items did not load/)).toBeTruthy();
      // The failure stops the loop: no further request until the retry.
      expect(pageTokens(api, "ListBillingAccounts")).toEqual(["", "p1"]);
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Billing accounts" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(screen.queryByText(/Some items did not load/)).toBeNull();
      expect(pageTokens(api, "ListBillingAccounts")).toEqual(["", "p1", "p1"]);
    });

    test("opens the last account of the second page", async () => {
      const api = createFakeApi();
      addBillingAccounts(api, 200);
      renderApp("/billing", { api });
      const link = await screen.findByRole("link", {
        name: new RegExp(`Account ${numbered(200)}`),
      });
      await settle(100);
      await click(link);
      expect(
        await screen.findByRole("heading", {
          name: `Account ${numbered(200)}`,
          level: 1,
        }),
      ).toBeTruthy();
    });
  });

  describe("members", () => {
    test("reads every page in server order with the controls of the last member", async () => {
      const api = createFakeApi();
      addMembers(api.accounts.get(ACCOUNT_ID)!, 198);
      await renderAccount(undefined, { api });
      await openTab("Members");
      const list = await screen.findByRole("list", { name: "Members" });
      await settle(100);
      const titles = rowTitles(list);
      expect(titles).toHaveLength(201);
      expect(titles[0]).toContain("Olivia Owner");
      expect(titles[200]).toContain(`Member ${numbered(198)}`);
      expect(calls(api, "ListMembers")).toEqual([
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: "",
        }),
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: "p1",
        }),
      ]);
      const last = within(list).getAllByRole("listitem")[200]!;
      expect(
        within(last).getByRole("button", { name: "Make owner" }),
      ).toBeTruthy();
      expect(
        within(last).getByRole("combobox", {
          name: `Role of Member ${numbered(198)}`,
        }),
      ).toBeTruthy();
      await click(within(last).getByRole("button", { name: "Remove" }));
      const dialog = await screen.findByRole("alertdialog");
      await click(
        within(dialog).getByRole("button", { name: "Remove member" }),
      );
      await settle(100);
      expect(calls(api, "RemoveMember")).toEqual([
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          userId: `user-${numbered(198)}`,
        }),
      ]);
    });

    test("the last member can receive ownership", async () => {
      const api = createFakeApi();
      addMembers(api.accounts.get(ACCOUNT_ID)!, 198);
      await renderAccount(undefined, { api });
      await openTab("Members");
      const list = await screen.findByRole("list", { name: "Members" });
      await settle(100);
      const last = within(list).getAllByRole("listitem")[200]!;
      await click(within(last).getByRole("button", { name: "Make owner" }));
      const dialog = await screen.findByRole("alertdialog");
      await click(
        within(dialog).getByRole("button", { name: "Transfer ownership" }),
      );
      await settle(100);
      expect(calls(api, "TransferOwnership")).toEqual([
        expect.objectContaining({ userId: `user-${numbered(198)}` }),
      ]);
    });

    test("an exact page and an empty token make no continuation request", async () => {
      const api = createFakeApi();
      addMembers(api.accounts.get(ACCOUNT_ID)!, 197);
      await renderAccount(undefined, { api });
      await openTab("Members");
      const list = await screen.findByRole("list", { name: "Members" });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      expect(pageTokens(api, "ListMembers")).toEqual([""]);
    });

    test("a failed continuation retries without duplicates and a viewer stays read-only", async () => {
      const api = createFakeApi();
      addMembers(api.accounts.get(ACCOUNT_ID)!, 198);
      api.failContinuation.set(
        "ListMembers",
        new ConnectError("temporarily unavailable", Code.Unavailable),
      );
      await renderAccount(VIEWER, { api });
      await openTab("Members");
      const list = await screen.findByRole("list", { name: "Members" });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Members" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(pageTokens(api, "ListMembers")).toEqual(["", "p1", "p1"]);
      const members = screen.getByRole("list", { name: "Members" });
      expect(within(members).queryByRole("button")).toBeNull();
      expect(within(members).queryByRole("combobox")).toBeNull();
    });
  });

  describe("organizations", () => {
    test("reads every page in server order, including the last row", async () => {
      const api = createFakeApi();
      addLinkedOrganizations(api.accounts.get(ACCOUNT_ID)!, 200);
      await renderAccount(undefined, { api });
      await openTab("Organizations");
      const list = await screen.findByRole("list", {
        name: "Linked organizations",
      });
      await settle(100);
      const titles = rowTitles(list);
      expect(titles).toHaveLength(201);
      expect(titles[0]).toBe("Acme sandboxes");
      expect(titles[200]).toBe(`Org ${numbered(200)}`);
      const last = within(list).getAllByRole("listitem")[200]!;
      expect(last.textContent).toContain("OwnSand");
      expect(last.textContent).toContain("Active");
      expect(calls(api, "ListLinkedOrganizations")).toEqual([
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: "",
        }),
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: "p1",
        }),
      ]);
    });

    test("an exact page and an empty token make no continuation request", async () => {
      const api = createFakeApi();
      addLinkedOrganizations(api.accounts.get(ACCOUNT_ID)!, 199);
      await renderAccount(undefined, { api });
      await openTab("Organizations");
      const list = await screen.findByRole("list", {
        name: "Linked organizations",
      });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      expect(pageTokens(api, "ListLinkedOrganizations")).toEqual([""]);
    });

    test("no organization keeps the empty state", async () => {
      const api = createFakeApi();
      api.accounts.get(ACCOUNT_ID)!.linked = [];
      await renderAccount(undefined, { api });
      await openTab("Organizations");
      expect(await screen.findByText("No linked organizations")).toBeTruthy();
      await settle(100);
      expect(pageTokens(api, "ListLinkedOrganizations")).toEqual([""]);
    });

    test("a failed continuation retries without duplicates", async () => {
      const api = createFakeApi();
      addLinkedOrganizations(api.accounts.get(ACCOUNT_ID)!, 200);
      api.failContinuation.set(
        "ListLinkedOrganizations",
        new ConnectError("temporarily unavailable", Code.Unavailable),
      );
      await renderAccount(undefined, { api });
      await openTab("Organizations");
      const list = await screen.findByRole("list", {
        name: "Linked organizations",
      });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Linked organizations" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(pageTokens(api, "ListLinkedOrganizations")).toEqual([
        "",
        "p1",
        "p1",
      ]);
    });

    test("switching to another billing account shows only its organizations", async () => {
      const api = createFakeApi();
      const first = api.accounts.get(ACCOUNT_ID)!;
      addLinkedOrganizations(first, 200, "Alpha");
      addBillingAccounts(api, 1);
      const [secondId, second] = [...api.accounts].find(
        ([, account]) => account.name === `Account ${numbered(1)}`,
      )!;
      addLinkedOrganizations(second, 2, "Beta");
      await renderAccount(undefined, { api });
      await openTab("Organizations");
      const list = await screen.findByRole("list", {
        name: "Linked organizations",
      });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(201);
      await act(async () => {
        window.history.pushState(null, "", `/billing/${secondId}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
      });
      await screen.findByRole("heading", {
        name: `Account ${numbered(1)}`,
        level: 1,
      });
      await openTab("Organizations");
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Linked organizations" }),
      );
      expect(titles).toEqual([`Beta ${numbered(1)}`, `Beta ${numbered(2)}`]);
      expect(calls(api, "ListLinkedOrganizations").at(-1)).toMatchObject({
        billingAccountId: secondId,
        pageToken: "",
      });
    });
  });

  describe("invitations", () => {
    test("reads every page with a Revoke action for the last invitation", async () => {
      const api = createFakeApi();
      addInvitations(api.accounts.get(ACCOUNT_ID)!, 200);
      await renderAccount(undefined, { api });
      await openTab("Members");
      const list = await screen.findByRole("list", {
        name: "Pending invitations",
      });
      await settle(100);
      const titles = rowTitles(list);
      expect(titles).toHaveLength(201);
      expect(titles[0]).toBe("pending@example.com");
      expect(titles[200]).toBe(`invitee-${numbered(200)}@example.com`);
      expect(
        within(list).getAllByRole("button", { name: "Revoke" }),
      ).toHaveLength(201);
      expect(calls(api, "ListInvitations")).toEqual([
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: "",
        }),
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: "p1",
        }),
      ]);
      const last = within(list).getAllByRole("listitem")[200]!;
      await click(within(last).getByRole("button", { name: "Revoke" }));
      await settle(100);
      expect(calls(api, "RevokeInvitation")).toEqual([
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          invitationId: `0193a1b2-0002-7000-8000-${"200".padStart(12, "0")}`,
        }),
      ]);
    });

    test("an exact page and an empty token make no continuation request", async () => {
      const api = createFakeApi();
      addInvitations(api.accounts.get(ACCOUNT_ID)!, 199);
      await renderAccount(undefined, { api });
      await openTab("Members");
      const list = await screen.findByRole("list", {
        name: "Pending invitations",
      });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      expect(pageTokens(api, "ListInvitations")).toEqual([""]);
    });

    test("no pending invitation keeps the empty state", async () => {
      const api = createFakeApi();
      api.accounts.get(ACCOUNT_ID)!.invitations = [];
      await renderAccount(undefined, { api });
      await openTab("Members");
      expect(await screen.findByText("No pending invitations")).toBeTruthy();
      await settle(100);
      expect(pageTokens(api, "ListInvitations")).toEqual([""]);
    });

    test("a failed continuation retries without duplicates", async () => {
      const api = createFakeApi();
      addInvitations(api.accounts.get(ACCOUNT_ID)!, 200);
      api.failContinuation.set(
        "ListInvitations",
        new ConnectError("temporarily unavailable", Code.Unavailable),
      );
      await renderAccount(undefined, { api });
      await openTab("Members");
      const list = await screen.findByRole("list", {
        name: "Pending invitations",
      });
      await settle(100);
      expect(rowTitles(list)).toHaveLength(200);
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Pending invitations" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(pageTokens(api, "ListInvitations")).toEqual(["", "p1", "p1"]);
    });
  });
});
