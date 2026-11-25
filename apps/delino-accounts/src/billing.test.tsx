// SPDX-License-Identifier: Apache-2.0
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { BillingAccountRole } from "@delino/accounts-protos/v1/billing_account_pb";
import { SubscriptionStatus } from "@delino/accounts-protos/v1/credit_pb";
import { pendingInvitation } from "./invitation.ts";
import { afterSignInPath } from "./pages/callback.tsx";
import {
  ACCOUNT_ID,
  ADMIN,
  addBillingAccounts,
  addInvitations,
  addInvoices,
  addLedgerEntries,
  addLinkedOrganizations,
  addMembers,
  addUsageRows,
  createFakeApi,
  INVITATION_TOKEN,
  numbered,
  OUTSIDER,
  OWNER,
  VIEWER,
} from "./testing/fake-api.ts";
import { deferNextCall } from "./testing/form-submit-deferral.ts";
import { listItems } from "./testing/list-rows.ts";
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
  rs.useRealTimers();
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

  /** Renames the account in the fake API, like another member would. */
  function renameElsewhere(api: ReturnType<typeof createFakeApi>, to: string) {
    api.accounts.get(ACCOUNT_ID)!.name = to;
  }

  /** The Billing account name field of the Settings tab. */
  function nameField() {
    return screen.getByRole("textbox", {
      name: "Billing account name",
    }) as HTMLInputElement;
  }

  function saveNameButton() {
    return within(nameField().closest("form")!).getByRole("button", {
      name: "Save",
    }) as HTMLButtonElement;
  }

  test("an unedited name field and the header follow a refetched account name", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    expect(nameField().value).toBe("Acme Robotics");

    renameElsewhere(api, "Beta");
    await refocus();

    expect(
      await screen.findByRole("heading", { name: "Beta", level: 1 }),
    ).toBeTruthy();
    expect(nameField().value).toBe("Beta");
    await click(
      within(nameField().closest("form")!).getByRole("button", {
        name: "Save",
      }),
    );
    await settle(60);
    expect(calls(api, "UpdateBillingAccount")).toEqual([
      expect.objectContaining({ name: "Beta" }),
    ]);
  });

  test("the read-only name field of a viewer follows a refetched account name", async () => {
    const { api } = await renderAccount(VIEWER);
    await openTab("Settings");
    expect(nameField().disabled).toBe(true);

    renameElsewhere(api, "Beta");
    await refocus();

    expect(nameField().value).toBe("Beta");
    expect(nameField().disabled).toBe(true);
  });

  test("a refetch keeps an unsaved name edit and a later save sends it", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    await type(nameField(), "Draft");

    renameElsewhere(api, "Beta");
    await refocus();

    expect(
      await screen.findByRole("heading", { name: "Beta", level: 1 }),
    ).toBeTruthy();
    expect(nameField().value).toBe("Draft");
    await click(
      within(nameField().closest("form")!).getByRole("button", {
        name: "Save",
      }),
    );
    await settle(60);
    expect(calls(api, "UpdateBillingAccount")).toEqual([
      expect.objectContaining({ name: "Draft" }),
    ]);
  });

  for (const [draft, nextName] of [
    ["  Beta  ", "Beta"],
    ["Acme Robotics", "Acme Robotics"],
  ]) {
    test(`keeps the newer name draft "${draft}" through a successful save and refetch`, async () => {
      const { api } = await renderAccount(ADMIN);
      await openTab("Settings");
      const pending = deferNextCall(api, "UpdateBillingAccount");
      await type(nameField(), "  Alpha  ");
      await click(saveNameButton());
      expect(calls(api, "UpdateBillingAccount")).toEqual([
        expect.objectContaining({ name: "Alpha" }),
      ]);
      expect(saveNameButton().disabled).toBe(true);
      expect(nameField().disabled).toBe(false);
      await type(nameField(), draft);

      pending.release();
      await screen.findByRole("heading", { name: "Alpha", level: 1 });
      expect(nameField().value).toBe(draft);
      await waitFor(() => expect(saveNameButton().disabled).toBe(false));
      await click(saveNameButton());
      await screen.findByRole("heading", { name: nextName, level: 1 });
      expect(calls(api, "UpdateBillingAccount")).toEqual([
        expect.objectContaining({ name: "Alpha" }),
        expect.objectContaining({ name: nextName }),
      ]);
    });
  }

  test("a failed name save keeps the latest edit for a retry", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    const pending = deferNextCall(api, "UpdateBillingAccount");
    await type(nameField(), "Alpha");
    await click(saveNameButton());
    await type(nameField(), "  Gamma  ");
    pending.fail(BACKGROUND_FAILURE);
    await screen.findByText(BUSY);
    expect(nameField().value).toBe("  Gamma  ");

    await click(saveNameButton());
    await screen.findByRole("heading", { name: "Gamma", level: 1 });
    expect(calls(api, "UpdateBillingAccount")).toEqual([
      expect.objectContaining({ name: "Alpha" }),
      expect.objectContaining({ name: "Gamma" }),
    ]);
    expect(nameField().value).toBe("Gamma");
  });

  test("shows the normalized saved name during and after a failed refetch", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    const refetch = deferNextCall(api, "GetBillingAccount");
    await type(nameField(), "  Gamma  ");
    await click(saveNameButton());
    await waitFor(() => expect(nameField().value).toBe("Gamma"));
    expect(
      screen.getByRole("heading", { name: "Acme Robotics", level: 1 }),
    ).toBeTruthy();
    expect(saveNameButton().disabled).toBe(true);

    refetch.fail(BACKGROUND_FAILURE);
    await screen.findByText(BUSY);
    expect(nameField().value).toBe("Gamma");

    await click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByRole("heading", { name: "Gamma", level: 1 });
    renameElsewhere(api, "Beta");
    await refocus();
    expect(nameField().value).toBe("Beta");
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

describe("pending subscription polling", () => {
  /** The subscription poll interval of the overview. */
  const POLL_MS = 2000;
  /** Enough fake time for the loading skeleton delay and the first reads. */
  const SETTLE_MS = 100;

  /** Runs the fake clock forward inside `act`, flushing pending requests. */
  async function advance(ms: number) {
    await act(async () => {
      await rs.advanceTimersByTimeAsync(ms);
      await new Promise((resolve) =>
        rs.getRealTimers().setTimeout(resolve, 20),
      );
    });
  }

  /** Renders the overview under fake timers and lets the first reads finish. */
  async function renderPending(
    status: SubscriptionStatus,
    options: {
      userId?: string;
      checkout?: ReturnType<typeof fakeCheckout>;
    } = {},
  ) {
    const api = createFakeApi();
    api.userId = options.userId ?? OWNER;
    api.accounts.get(ACCOUNT_ID)!.subscription = {
      status,
      cancelAtPeriodEnd: false,
    };
    rs.useFakeTimers({
      toFake: [
        "setTimeout",
        "clearTimeout",
        "setInterval",
        "clearInterval",
        "Date",
      ],
    });
    renderApp(ACCOUNT_PATH, { api, checkout: options.checkout });
    await advance(SETTLE_MS);
    await advance(SETTLE_MS);
    return api;
  }

  /** Confirms the payment on the server like the verified webhook does. */
  function confirm(api: ReturnType<typeof createFakeApi>) {
    const account = api.accounts.get(ACCOUNT_ID)!;
    account.subscription = {
      status: SubscriptionStatus.ACTIVE,
      cancelAtPeriodEnd: false,
      periodEnd: new Date("2026-12-01T00:00:00Z"),
    };
    account.balance += 10_000_000n;
  }

  /** The balance text without `waitFor`, which cannot run on fake timers. */
  const balanceNow = () =>
    screen.getByRole("region", { name: "Balance" }).querySelector(".balance")
      ?.textContent;

  const topUpDisabled = () =>
    (screen.getByRole("button", { name: "Top up" }) as HTMLButtonElement)
      .disabled;

  test("a delayed confirmation updates the page without a reload and then stops polling", async () => {
    const api = await renderPending(SubscriptionStatus.PENDING);
    expect(screen.getByText("Waiting for payment")).toBeTruthy();
    expect(topUpDisabled()).toBe(true);
    expect(balanceNow()).toBe("$12.35");

    // Still PENDING: the page reads the subscription every 2 s and nothing else.
    api.calls.length = 0;
    await advance(POLL_MS);
    await advance(POLL_MS);
    expect(calls(api, "GetSubscription")).toHaveLength(2);
    // Every read stays on the account of the page.
    expect(
      calls(api, "GetSubscription").map((r) => r.billingAccountId),
    ).toEqual([ACCOUNT_ID, ACCOUNT_ID]);
    expect(calls(api, "GetBalance")).toHaveLength(0);
    expect(screen.getByText("Waiting for payment")).toBeTruthy();

    // The webhook confirms the payment between two polls.
    confirm(api);
    api.calls.length = 0;
    await advance(POLL_MS);
    expect(screen.queryByText("Waiting for payment")).toBeNull();
    expect(topUpDisabled()).toBe(false);
    expect(balanceNow()).toBe("$22.35");
    expect(calls(api, "GetBalance").length).toBeGreaterThan(0);

    // The transition invalidated the credit reads once; no timer remains.
    await advance(POLL_MS);
    api.calls.length = 0;
    await advance(POLL_MS * 5);
    expect(api.calls).toEqual([]);
  });

  test("an admin can top up after the confirmation", async () => {
    const api = await renderPending(SubscriptionStatus.PENDING, {
      userId: ADMIN,
    });
    expect(topUpDisabled()).toBe(true);
    confirm(api);
    await advance(POLL_MS);
    expect(topUpDisabled()).toBe(false);
  });

  test("a reload while PENDING polls from the initial load", async () => {
    const api = await renderPending(SubscriptionStatus.PENDING);
    expect(calls(api, "GetSubscription")).toHaveLength(1);
    await advance(POLL_MS);
    expect(calls(api, "GetSubscription")).toHaveLength(2);
    confirm(api);
    await advance(POLL_MS);
    expect(screen.queryByText("Waiting for payment")).toBeNull();
  });

  test("no timer is scheduled for a subscription that is not PENDING", async () => {
    for (const status of [
      SubscriptionStatus.ACTIVE,
      SubscriptionStatus.NONE,
      SubscriptionStatus.CANCELED,
    ]) {
      const api = await renderPending(status);
      api.calls.length = 0;
      await advance(POLL_MS * 5);
      expect(api.calls).toEqual([]);
      cleanup();
      rs.useRealTimers();
    }
  });

  test("a confirmation before the checkout closes does not poll or refresh twice", async () => {
    const checkout = fakeCheckout();
    const api = await renderPending(SubscriptionStatus.NONE, { checkout });
    await act(async () => {
      fireEvent.click(
        screen.getByRole("button", { name: "Start subscription" }),
      );
    });
    await advance(SETTLE_MS);
    await advance(SETTLE_MS);
    expect(checkout.opened).toEqual(["txn_checkout1"]);
    // The webhook arrives before the poll runs and before the close callback.
    confirm(api);
    await act(async () => checkout.close());
    await advance(SETTLE_MS);
    await advance(SETTLE_MS);
    expect(screen.queryByText("Waiting for payment")).toBeNull();
    expect(topUpDisabled()).toBe(false);
    api.calls.length = 0;
    await advance(POLL_MS * 5);
    expect(api.calls).toEqual([]);
  });

  test("an unpaid closed checkout stays pending with reads only", async () => {
    const checkout = fakeCheckout();
    const api = await renderPending(SubscriptionStatus.PENDING, { checkout });
    api.calls.length = 0;
    await advance(POLL_MS * 5);
    expect(screen.getByText("Waiting for payment")).toBeTruthy();
    expect(topUpDisabled()).toBe(true);
    const methods = new Set(api.calls.map((c) => c.method));
    expect(methods).toEqual(new Set(["GetSubscription"]));
    expect(calls(api, "GetSubscription")).toHaveLength(5);
    expect(calls(api, "StartSubscription")).toHaveLength(0);
    expect(calls(api, "CreateTopUp")).toHaveLength(0);
    expect(checkout.opened).toEqual([]);
  });

  test("unmounting stops the polling", async () => {
    const api = await renderPending(SubscriptionStatus.PENDING);
    await advance(POLL_MS);
    cleanup();
    api.calls.length = 0;
    await advance(POLL_MS * 5);
    expect(api.calls).toEqual([]);
  });

  test("a transient Unavailable during polling does not stop it", async () => {
    const api = await renderPending(SubscriptionStatus.PENDING);
    api.failNext.set(
      "GetSubscription",
      new ConnectError("try again", Code.Unavailable),
    );
    await advance(POLL_MS);
    // The next poll succeeds and sees the confirmation.
    confirm(api);
    await advance(POLL_MS);
    await advance(SETTLE_MS);
    expect(screen.queryByText("Waiting for payment")).toBeNull();
    expect(topUpDisabled()).toBe(false);
    expect(balanceNow()).toBe("$22.35");
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
    // Pin only the clock, at the first day of a UTC month that follows a
    // different year, so the component, the fake API and the assertions agree
    // on the month. Timers stay real so requests and effects settle normally;
    // the file-level afterEach restores the real clock, also on a failure.
    rs.useFakeTimers({
      now: new Date("2027-01-01T12:00:00Z"),
      toFake: ["Date"],
    });
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
    expect(months[0]).toEqual(
      expect.objectContaining({ year: 2027, month: 1 }),
    );
    expect(months.at(-1)).toEqual(
      expect.objectContaining({ year: 2026, month: 12 }),
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
    expect(
      screen
        .getByRole("tab", { name: "Overview" })
        .getAttribute("aria-selected"),
    ).toBe("true");
  });

  /** Changes the caller's role in the fake API and refetches the account. */
  async function changeRoleAndRefetch(
    api: ReturnType<typeof createFakeApi>,
    userId: string,
    role: BillingAccountRole,
  ) {
    const member = api.accounts
      .get(ACCOUNT_ID)
      ?.members.find((m) => m.userId === userId);
    if (member === undefined) throw new Error("member missing");
    member.role = role;
    await act(async () => {
      window.dispatchEvent(new Event("visibilitychange"));
    });
    await settle();
  }

  test("Overview stays selected after invoice access returns", async () => {
    const api = createFakeApi();
    await renderAccount(ADMIN, { api });
    await openTab("Invoices");
    expect(
      screen
        .getByRole("tab", { name: "Invoices" })
        .getAttribute("aria-selected"),
    ).toBe("true");

    await changeRoleAndRefetch(api, ADMIN, BillingAccountRole.VIEWER);

    expect(screen.queryByRole("tab", { name: "Invoices" })).toBeNull();
    expect(screen.queryByRole("tabpanel", { name: "Invoices" })).toBeNull();
    expect(screen.getAllByRole("tab", { selected: true })).toEqual([
      screen.getByRole("tab", { name: "Overview" }),
    ]);

    await changeRoleAndRefetch(api, ADMIN, BillingAccountRole.ADMIN);

    expect(
      screen.getByRole("tab", { name: "Invoices", selected: false }),
    ).toBeTruthy();
    expect(screen.getAllByRole("tab", { selected: true })).toEqual([
      screen.getByRole("tab", { name: "Overview" }),
    ]);
    expect(screen.getByRole("tabpanel", { name: "Overview" })).toBeTruthy();

    await openTab("Invoices");

    expect(
      screen.getByRole("tab", { name: "Invoices", selected: true }),
    ).toBeTruthy();
  });

  test("a role change that keeps access keeps the Invoices tab selected", async () => {
    const api = createFakeApi();
    await renderAccount(undefined, { api });
    await openTab("Invoices");

    await changeRoleAndRefetch(api, OWNER, BillingAccountRole.ADMIN);

    expect(screen.getByText("Your role: Admin")).toBeTruthy();
    expect(
      screen
        .getByRole("tab", { name: "Invoices" })
        .getAttribute("aria-selected"),
    ).toBe("true");
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

  describe("while the link is in flight", () => {
    const INVOICE_URL = "https://sandbox-invoice.paddle.test/txn_top1.pdf";

    /**
     * Stands in for the blank tab that the click reserves. `closed` flips
     * when the test plays the user closing the tab.
     */
    function fakeTab() {
      const tab = {
        opener: "app" as unknown,
        closed: false,
        replaced: [] as string[],
        closeCalls: 0,
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
      return tab;
    }

    /**
     * Opens the invoice with the given `window.open` result and holds the
     * GetInvoiceUrl answer until `respond` runs. The location assignment
     * that a blocked pop-up falls back to is recorded and not performed.
     * The stubs stay installed until the caller runs `restore()`; when the
     * setup itself fails, they are restored here and the failure propagates
     * because the caller never receives a harness to restore.
     */
    async function openInvoiceWith(
      opened: ReturnType<typeof fakeTab> | null,
      failure?: ConnectError,
      api = createFakeApi(),
    ) {
      const originalOpen = window.open;
      const originalAssign = window.location.assign;
      const restore = () => {
        window.open = originalOpen;
        window.location.assign = originalAssign;
      };
      const assigned: string[] = [];
      window.open = (() => opened) as unknown as typeof window.open;
      window.location.assign = (url: string | URL) => {
        assigned.push(String(url));
      };
      try {
        if (failure !== undefined) api.failNext.set("GetInvoiceUrl", failure);
        let respond!: () => void;
        api.gates.set(
          "GetInvoiceUrl",
          new Promise<void>((resolve) => {
            respond = resolve;
          }),
        );
        await renderAccount(ADMIN, { api });
        await openTab("Invoices");
        const list = await screen.findByRole("list", { name: "Invoices" });
        await click(
          within(list).getByRole("button", { name: /Open the invoice/ }),
        );
        await settle(60);
        expect(calls(api, "GetInvoiceUrl")).toHaveLength(1);
        return {
          api,
          assigned,
          respond: async () => {
            respond();
            await settle(60);
          },
          restore,
        };
      } catch (error) {
        restore();
        throw error;
      }
    }

    test("a failed setup restores the browser stubs and propagates the failure", async () => {
      const originalOpen = window.open;
      const originalAssign = window.location.assign;
      const api = createFakeApi();
      api.failNext.set(
        "ListInvoices",
        new ConnectError("unavailable", Code.Unavailable),
      );
      await expect(
        openInvoiceWith(fakeTab(), undefined, api),
      ).rejects.toBeDefined();
      expect(window.open).toBe(originalOpen);
      expect(window.location.assign).toBe(originalAssign);
    });

    test("a successful setup keeps the browser stubs until restore", async () => {
      const originalOpen = window.open;
      const originalAssign = window.location.assign;
      const harness = await openInvoiceWith(fakeTab());
      try {
        expect(window.open).not.toBe(originalOpen);
        expect(window.location.assign).not.toBe(originalAssign);
      } finally {
        harness.restore();
      }
      expect(window.open).toBe(originalOpen);
      expect(window.location.assign).toBe(originalAssign);
    });

    test("a tab the user closed discards the link and leaves the billing page alone", async () => {
      const tab = fakeTab();
      const harness = await openInvoiceWith(tab);
      try {
        const pathBefore = window.location.pathname;
        tab.closed = true;
        await harness.respond();
        expect(tab.replaced).toEqual([]);
        expect(harness.assigned).toEqual([]);
        expect(window.location.pathname).toBe(pathBefore);
        expect(
          screen.getByRole("heading", { name: "Acme Robotics" }),
        ).toBeTruthy();
      } finally {
        harness.restore();
      }
    });

    test("a tab that stays open receives the invoice", async () => {
      const tab = fakeTab();
      const harness = await openInvoiceWith(tab);
      try {
        await harness.respond();
        expect(tab.replaced).toEqual([INVOICE_URL]);
        expect(tab.opener).toBeNull();
        expect(harness.assigned).toEqual([]);
      } finally {
        harness.restore();
      }
    });

    test("a blocked pop-up opens the invoice in the billing tab", async () => {
      const harness = await openInvoiceWith(null);
      try {
        expect(harness.assigned).toEqual([]);
        await harness.respond();
        expect(harness.assigned).toEqual([INVOICE_URL]);
      } finally {
        harness.restore();
      }
    });

    test.each([false, true])(
      "a blocked pop-up cannot redirect after leaving the account (reopen: %s)",
      async (reopen) => {
        const harness = await openInvoiceWith(null);
        try {
          await click(screen.getByRole("link", { name: "Billing accounts" }));
          const account = await screen.findByRole("link", {
            name: /Acme Robotics/,
          });
          if (reopen) {
            await click(account);
            await openTab("Invoices");
          }

          await harness.respond();

          expect(harness.assigned).toEqual([]);
          expect(window.location.pathname).toBe(
            reopen ? ACCOUNT_PATH : "/billing",
          );
          const heading = reopen ? "Invoices" : "Billing accounts";
          expect(screen.getByRole("heading", { name: heading })).toBeTruthy();
        } finally {
          harness.restore();
        }
      },
    );

    test("a blocked pop-up cannot redirect after switching to another panel", async () => {
      const harness = await openInvoiceWith(null);
      try {
        await openTab("Members");
        await screen.findByRole("heading", { name: "Members" });
        await harness.respond();

        expect(harness.assigned).toEqual([]);
        expect(screen.getByRole("heading", { name: "Members" })).toBeTruthy();
      } finally {
        harness.restore();
      }
    });

    test("a blocked pop-up cannot redirect after the caller loses invoice access", async () => {
      const harness = await openInvoiceWith(null);
      try {
        await changeRoleAndRefetch(
          harness.api,
          ADMIN,
          BillingAccountRole.VIEWER,
        );
        expect(screen.queryByRole("tab", { name: "Invoices" })).toBeNull();
        await harness.respond();

        expect(harness.assigned).toEqual([]);
        expect(screen.getByText("Your role: Viewer")).toBeTruthy();
      } finally {
        harness.restore();
      }
    });

    test("a blocked pop-up cannot redirect after another account replaces the page", async () => {
      const api = createFakeApi();
      api.userId = ADMIN;
      addBillingAccounts(api, 1);
      const replacement = [...api.accounts.values()].find(
        (account) => account.id !== ACCOUNT_ID,
      )!;
      const harness = await openInvoiceWith(null, undefined, api);
      try {
        await act(async () => {
          window.history.pushState(null, "", `/billing/${replacement.id}`);
          window.dispatchEvent(new PopStateEvent("popstate"));
        });
        await screen.findByRole("heading", { name: replacement.name });
        await openTab("Invoices");
        await harness.respond();

        expect(harness.assigned).toEqual([]);
        expect(window.location.pathname).toBe(`/billing/${replacement.id}`);
        expect(
          screen.getByRole("heading", { name: replacement.name }),
        ).toBeTruthy();
      } finally {
        harness.restore();
      }
    });

    test("an open pop-up receives the invoice after the user leaves the account", async () => {
      const tab = fakeTab();
      const harness = await openInvoiceWith(tab);
      try {
        await click(screen.getByRole("link", { name: "Billing accounts" }));
        await screen.findByRole("heading", { name: "Billing accounts" });
        await harness.respond();

        expect(tab.replaced).toEqual([INVOICE_URL]);
        expect(harness.assigned).toEqual([]);
      } finally {
        harness.restore();
      }
    });

    test("a failed request closes the open tab and shows the error toast", async () => {
      const tab = fakeTab();
      const harness = await openInvoiceWith(
        tab,
        new ConnectError("unavailable", Code.Unavailable),
      );
      try {
        expect(tab.closeCalls).toBe(1);
        expect(tab.replaced).toEqual([]);
        expect(harness.assigned).toEqual([]);
        expect(
          await screen.findByText(
            "Delino Accounts is not available right now. Try again in a moment.",
          ),
        ).toBeTruthy();
        const stored = JSON.stringify({
          ...window.sessionStorage,
          ...window.localStorage,
        });
        expect(stored).not.toContain("sandbox-invoice");
      } finally {
        harness.restore();
      }
    });
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

  test("an ended session shows a sign-in failure and keeps the token for a retry", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    const api = createFakeApi();
    api.userId = OUTSIDER;
    api.failNext.set(
      "AcceptInvitation",
      new ConnectError("token expired", Code.Unauthenticated),
    );
    const { signIns } = renderApp("/billing/invitations/accept", {
      api,
      signInFailed: true,
    });
    await click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
    await settle(60);
    expect(
      screen
        .getByText(
          "Sign-in did not start. Check your connection and try again.",
        )
        .getAttribute("role"),
    ).toBe("alert");
    const signIn = screen.getByRole("button", {
      name: "Sign in with Delino",
    }) as HTMLButtonElement;
    expect(signIn.disabled).toBe(false);
    await click(signIn);
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

  test("a replacement link opened while an earlier request is pending can be accepted once", async () => {
    const api = createFakeApi();
    api.userId = OUTSIDER;
    api.invitationTokens.set(EARLIER_INVITATION_TOKEN, {
      accountId: ACCOUNT_ID,
      role: BillingAccountRole.ADMIN,
    });
    // Hold the request of the earlier link so that it stays pending.
    let release!: () => void;
    api.gates.set(
      `AcceptInvitation:${EARLIER_INVITATION_TOKEN}`,
      new Promise<void>((resolve) => {
        release = resolve;
      }),
    );
    renderApp(`/billing/invitations/accept#token=${EARLIER_INVITATION_TOKEN}`, {
      api,
    });
    await click(
      await screen.findByRole("button", { name: "Accept invitation" }),
    );
    expect(calls(api, "AcceptInvitation")).toEqual([
      expect.objectContaining({ invitationToken: EARLIER_INVITATION_TOKEN }),
    ]);
    await act(async () => {
      window.location.hash = `token=${INVITATION_TOKEN}`;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await settle(30);
    const accept = screen.getByRole("button", {
      name: "Accept invitation",
    }) as HTMLButtonElement;
    expect(accept.disabled).toBe(false);
    expect(accept.getAttribute("aria-disabled")).not.toBe("true");
    // Two quick clicks send one request.
    await act(async () => {
      fireEvent.click(accept);
      fireEvent.click(accept);
    });
    await settle(60);
    expect(calls(api, "AcceptInvitation")).toEqual([
      expect.objectContaining({ invitationToken: EARLIER_INVITATION_TOKEN }),
      expect.objectContaining({ invitationToken: INVITATION_TOKEN }),
    ]);
    expect(window.location.pathname).toBe(ACCOUNT_PATH);
    // The late result of the earlier request changes nothing on screen.
    release();
    await settle(60);
    expect(window.location.pathname).toBe(ACCOUNT_PATH);
    expect(calls(api, "AcceptInvitation")).toHaveLength(2);
  });

  test.each([
    { outcome: "succeeds", registered: true },
    { outcome: "fails", registered: false },
  ])(
    "a replacement link keeps its stored token for the sign-in return when the earlier request $outcome meanwhile",
    async ({ registered }) => {
      const api = createFakeApi();
      api.userId = OUTSIDER;
      // The earlier link joins another account, so its late success cannot
      // block the acceptance of the replacement link.
      addBillingAccounts(api, 1);
      const earlierAccountId = [...api.accounts.keys()].find(
        (id) => id !== ACCOUNT_ID,
      )!;
      if (registered) {
        api.invitationTokens.set(EARLIER_INVITATION_TOKEN, {
          accountId: earlierAccountId,
          role: BillingAccountRole.ADMIN,
        });
      }
      // Hold the request of the earlier link so that it settles after the
      // replacement link started its sign-in redirect.
      let release!: () => void;
      api.gates.set(
        `AcceptInvitation:${EARLIER_INVITATION_TOKEN}`,
        new Promise<void>((resolve) => {
          release = resolve;
        }),
      );
      const { signIns } = renderApp(
        `/billing/invitations/accept#token=${EARLIER_INVITATION_TOKEN}`,
        { api },
      );
      await click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
      // The session of the replacement link has ended.
      api.failNext.set(
        "AcceptInvitation",
        new ConnectError("token expired", Code.Unauthenticated),
      );
      await act(async () => {
        window.location.hash = `token=${INVITATION_TOKEN}`;
        window.dispatchEvent(new HashChangeEvent("hashchange"));
      });
      await settle(30);
      await click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
      await settle(30);
      await click(screen.getByRole("button", { name: "Sign in with Delino" }));
      expect(signIns).toHaveLength(1);
      expect(pendingInvitation.read()).toBe(INVITATION_TOKEN);

      // The earlier request settles during the sign-in redirect.
      release();
      await settle(60);
      expect(pendingInvitation.read()).toBe(INVITATION_TOKEN);
      expect(window.location.pathname).toBe("/billing/invitations/accept");

      // The sign-in return opens the invitation route, and the next
      // acceptance submits the replacement token.
      expect(afterSignInPath()).toBe("/billing/invitations/accept");
      cleanup();
      renderApp("/billing/invitations/accept", { api });
      await click(
        await screen.findByRole("button", { name: "Accept invitation" }),
      );
      await settle(60);
      expect(calls(api, "AcceptInvitation")).toEqual([
        expect.objectContaining({ invitationToken: EARLIER_INVITATION_TOKEN }),
        expect.objectContaining({ invitationToken: INVITATION_TOKEN }),
        expect.objectContaining({ invitationToken: INVITATION_TOKEN }),
      ]);
      expect(window.location.pathname).toBe(ACCOUNT_PATH);
      expect(pendingInvitation.read()).toBeUndefined();
    },
  );

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

  test.each(["/billing", "/", ACCOUNT_PATH, "/unknown"])(
    "a fresh load of %s deletes the stored token of an abandoned redirect",
    async (path) => {
      pendingInvitation.save(INVITATION_TOKEN);
      renderApp(path, { signedIn: false });
      expect(pendingInvitation.read()).toBeUndefined();
      await settle();
      expect(pendingInvitation.read()).toBeUndefined();
    },
  );

  test("a fresh load of the invitation page keeps the stored token", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    renderApp("/billing/invitations/accept", { signedIn: false });
    await settle();
    expect(pendingInvitation.read()).toBe(INVITATION_TOKEN);
  });

  test("a fresh load of the invitation page under StrictMode keeps the stored token", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    renderApp("/billing/invitations/accept", { signedIn: false, strict: true });
    await settle();
    expect(pendingInvitation.read()).toBe(INVITATION_TOKEN);
  });

  test("a fresh load outside the flow under StrictMode deletes the stored token", async () => {
    pendingInvitation.save(INVITATION_TOKEN);
    renderApp("/billing", { signedIn: false, strict: true });
    await settle();
    expect(pendingInvitation.read()).toBeUndefined();
  });
});

describe("settings", () => {
  test("shows a queued automatic top-up as monthly cap usage", async () => {
    const api = createFakeApi();
    const account = api.accounts.get(ACCOUNT_ID)!;
    account.autoTopUp = {
      ...account.autoTopUp,
      monthToDateUsdMicros: 25_000_000n,
    };
    await renderAccount(ADMIN, { api });
    await openTab("Settings");

    expect(
      (await screen.findByText("Monthly cap usage")).nextSibling?.textContent,
    ).toBe("$25.00");
    expect(
      screen.getByText(/Automatic top-ups in progress count toward the cap\./),
    ).toBeTruthy();
    expect(screen.queryByText("Charged this month")).toBeNull();
  });

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

  test("a refetch that changes only the read-only counters keeps the unsaved automatic top-up draft", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    const amount = await screen.findByRole("textbox", {
      name: "Top-up amount (USD)",
    });
    await type(amount, "75");
    expect(screen.getByText("Monthly cap usage").nextSibling?.textContent).toBe(
      "$0.00",
    );
    const fake = api.accounts.get(ACCOUNT_ID)!;
    fake.autoTopUp = {
      ...fake.autoTopUp,
      monthToDateUsdMicros: 30_000_000n,
      consecutiveFailures: 1,
    };
    const before = calls(api, "GetAutoTopUpSettings").length;
    await act(async () => {
      window.dispatchEvent(new Event("visibilitychange"));
    });
    await settle(60);
    expect(calls(api, "GetAutoTopUpSettings").length).toBeGreaterThan(before);
    expect(screen.getByText("Monthly cap usage").nextSibling?.textContent).toBe(
      "$30.00",
    );
    expect(
      screen.getByText("Failed top-ups in a row").nextSibling?.textContent,
    ).toBe("1");
    expect(
      (
        screen.getByRole("textbox", {
          name: "Top-up amount (USD)",
        }) as HTMLInputElement
      ).value,
    ).toBe("75");
  });

  test("a refetch that changes a saved editable value resets the automatic top-up form", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    const amount = await screen.findByRole("textbox", {
      name: "Top-up amount (USD)",
    });
    await type(amount, "75");
    const fake = api.accounts.get(ACCOUNT_ID)!;
    fake.autoTopUp = { ...fake.autoTopUp, amountUsdMicros: 40_000_000n };
    await act(async () => {
      window.dispatchEvent(new Event("visibilitychange"));
    });
    await settle(60);
    expect(
      (
        screen.getByRole("textbox", {
          name: "Top-up amount (USD)",
        }) as HTMLInputElement
      ).value,
    ).toBe("40.00");
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

  test("rejects an automatic top-up threshold and monthly cap above the int64 bound without a request", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    await click(
      await screen.findByRole("switch", { name: "Automatic top-up" }),
    );
    const threshold = screen.getByRole("textbox", { name: "Threshold (USD)" });
    const amount = screen.getByRole("textbox", { name: "Top-up amount (USD)" });
    const cap = screen.getByRole("textbox", { name: "Monthly cap (USD)" });
    const save = within(threshold.closest("form")!).getByRole("button", {
      name: "Save",
    });
    await type(threshold, "99999999999999");
    await type(amount, "25");
    await type(cap, "99999999999999");
    await click(save);
    expect(
      screen.getAllByText("Enter at most $9,223,372,036,854.77."),
    ).toHaveLength(2);
    expect(calls(api, "UpdateAutoTopUpSettings")).toHaveLength(0);
    await type(threshold, "9223372036854.77");
    await type(cap, "9223372036854.77");
    await click(save);
    await settle(60);
    expect(calls(api, "UpdateAutoTopUpSettings")).toEqual([
      expect.objectContaining({
        settings: expect.objectContaining({
          thresholdUsdMicros: 9_223_372_036_854_770_000n,
          monthlyCapUsdMicros: 9_223_372_036_854_770_000n,
        }),
      }),
    ]);
  });

  test("rejects an alert threshold above the int64 bound without a request", async () => {
    const api = createFakeApi();
    await renderAccount(undefined, { api });
    await openTab("Settings");
    const input = await screen.findByRole("textbox", {
      name: "Alert threshold (USD)",
    });
    const save = within(input.closest("form")!).getByRole("button", {
      name: "Save",
    });
    await type(input, "9223372036854.78");
    await click(save);
    expect(
      screen.getByText("Enter at most $9,223,372,036,854.77."),
    ).toBeTruthy();
    expect(calls(api, "UpdateAlertSettings")).toHaveLength(0);
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

/**
 * The test timeout of the tests that render a 200-row list with a select
 * control in every row and open a dialog over it. The dialog re-renders every
 * row; the tests take about 2 s on an idle machine and 5 s on a loaded CI
 * runner, which is the default timeout. The bound is explicit so that CI load
 * cannot flake them; a hang still fails after this time.
 */
const HEAVY_LIST_TEST_TIMEOUT_MS = 20_000;

/** The titles of the rows of a list, in order. */
function rowTitles(list: HTMLElement) {
  return listItems(list).map(
    (row) => row.querySelector(".list-title")?.textContent ?? "",
  );
}

/** The continuation tokens that the requests of a method sent, in order. */
function pageTokens(api: ReturnType<typeof createFakeApi>, method: string) {
  return calls(api, method).map((request) => request.pageToken);
}

const nonemptyPageToken = expect.stringMatching(/./);

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
      expect(pageTokens(api, "ListBillingAccounts")).toEqual([
        "",
        nonemptyPageToken,
      ]);
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
      const requestsBeforeRetry = pageTokens(api, "ListBillingAccounts");
      expect(requestsBeforeRetry).toEqual(["", nonemptyPageToken]);
      const failedToken = requestsBeforeRetry[1];
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Billing accounts" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(screen.queryByText(/Some items did not load/)).toBeNull();
      expect(pageTokens(api, "ListBillingAccounts")).toEqual([
        ...requestsBeforeRetry,
        failedToken,
      ]);
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
    test(
      "reads every page in server order with the controls of the last member",
      async () => {
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
            pageToken: nonemptyPageToken,
          }),
        ]);
        const last = listItems(list)[200]!;
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
      },
      HEAVY_LIST_TEST_TIMEOUT_MS,
    );

    test(
      "the last member can receive ownership",
      async () => {
        const api = createFakeApi();
        addMembers(api.accounts.get(ACCOUNT_ID)!, 198);
        await renderAccount(undefined, { api });
        await openTab("Members");
        const list = await screen.findByRole("list", { name: "Members" });
        await settle(100);
        const last = listItems(list)[200]!;
        await click(within(last).getByRole("button", { name: "Make owner" }));
        const dialog = await screen.findByRole("alertdialog");
        await click(
          within(dialog).getByRole("button", { name: "Transfer ownership" }),
        );
        await settle(100);
        expect(calls(api, "TransferOwnership")).toEqual([
          expect.objectContaining({ userId: `user-${numbered(198)}` }),
        ]);
      },
      HEAVY_LIST_TEST_TIMEOUT_MS,
    );

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
      const requestsBeforeRetry = pageTokens(api, "ListMembers");
      expect(requestsBeforeRetry).toEqual(["", nonemptyPageToken]);
      const failedToken = requestsBeforeRetry[1];
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Members" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(pageTokens(api, "ListMembers")).toEqual([
        ...requestsBeforeRetry,
        failedToken,
      ]);
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
      const last = listItems(list)[200]!;
      expect(last.textContent).toContain("OwnSand");
      expect(last.textContent).toContain("Active");
      expect(calls(api, "ListLinkedOrganizations")).toEqual([
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: "",
        }),
        expect.objectContaining({
          billingAccountId: ACCOUNT_ID,
          pageToken: nonemptyPageToken,
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
      const requestsBeforeRetry = pageTokens(api, "ListLinkedOrganizations");
      expect(requestsBeforeRetry).toEqual(["", nonemptyPageToken]);
      const failedToken = requestsBeforeRetry[1];
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Linked organizations" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(pageTokens(api, "ListLinkedOrganizations")).toEqual([
        ...requestsBeforeRetry,
        failedToken,
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
          pageToken: nonemptyPageToken,
        }),
      ]);
      const last = listItems(list)[200]!;
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
      const requestsBeforeRetry = pageTokens(api, "ListInvitations");
      expect(requestsBeforeRetry).toEqual(["", nonemptyPageToken]);
      const failedToken = requestsBeforeRetry[1];
      await click(screen.getByRole("button", { name: "Try again" }));
      await settle(100);
      const titles = rowTitles(
        await screen.findByRole("list", { name: "Pending invitations" }),
      );
      expect(titles).toHaveLength(201);
      expect(new Set(titles).size).toBe(201);
      expect(pageTokens(api, "ListInvitations")).toEqual([
        ...requestsBeforeRetry,
        failedToken,
      ]);
    });
  });
});

/** Triggers the refetch of every stale query, like a return to the tab. */
async function refocus() {
  await act(async () => {
    window.dispatchEvent(new Event("visibilitychange"));
  });
  await settle(60);
}

/** The notice text of a ResourceExhausted failure. */
const BUSY = "Too many requests. Wait a moment and try again.";
const BACKGROUND_FAILURE = new ConnectError("busy", Code.ResourceExhausted);

describe("failed background refetches", () => {
  test("a failed refetch of the account keeps the page, the tab and the typed input", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Settings");
    const amount = await screen.findByRole("textbox", {
      name: "Top-up amount (USD)",
    });
    await type(amount, "75");
    api.failNext.set("GetBillingAccount", BACKGROUND_FAILURE);
    await refocus();
    expect(screen.getByText(BUSY)).toBeTruthy();
    expect(
      screen.getByRole("heading", { name: "Acme Robotics", level: 1 }),
    ).toBeTruthy();
    expect(
      screen
        .getByRole("tab", { name: "Settings" })
        .getAttribute("aria-selected"),
    ).toBe("true");
    expect(
      (
        screen.getByRole("textbox", {
          name: "Top-up amount (USD)",
        }) as HTMLInputElement
      ).value,
    ).toBe("75");

    // Try again reads the account again and clears the notice.
    const before = calls(api, "GetBillingAccount").length;
    await click(screen.getByRole("button", { name: "Try again" }));
    await settle(60);
    expect(calls(api, "GetBillingAccount").length).toBe(before + 1);
    expect(screen.queryByText(BUSY)).toBeNull();
    expect(
      (
        screen.getByRole("textbox", {
          name: "Top-up amount (USD)",
        }) as HTMLInputElement
      ).value,
    ).toBe("75");
  });

  test("a failed refetch of the account keeps an open dialog and its input", async () => {
    const { api } = await renderAccount(ADMIN);
    await openTab("Members");
    await click(await screen.findByRole("button", { name: "Invite by email" }));
    await settle(60);
    await type(
      screen.getByRole("textbox", { name: "Email" }),
      "new@example.com",
    );
    api.failNext.set("GetBillingAccount", BACKGROUND_FAILURE);
    await refocus();
    expect(screen.getByText(BUSY)).toBeTruthy();
    expect(
      (screen.getByRole("textbox", { name: "Email" }) as HTMLInputElement)
        .value,
    ).toBe("new@example.com");
  });

  for (const code of [Code.NotFound, Code.PermissionDenied]) {
    test(`a ${Code[code]} refetch of the account replaces the page`, async () => {
      const { api } = await renderAccount(ADMIN);
      api.failNext.set(
        "GetBillingAccount",
        new ConnectError("the account is gone", code),
      );
      await refocus();
      expect(
        screen.getByText(
          code === Code.NotFound
            ? "It was not found, or you no longer have access to it."
            : "the account is gone",
        ),
      ).toBeTruthy();
      expect(
        screen
          .getByRole("link", { name: "Go to your billing accounts" })
          .getAttribute("href"),
      ).toBe("/billing");
      expect(screen.queryByRole("tab", { name: "Settings" })).toBeNull();
    });
  }

  test("a failed first load shows only the notice", async () => {
    const api = createFakeApi();
    api.failNext.set("GetBillingAccount", BACKGROUND_FAILURE);
    renderApp(ACCOUNT_PATH, { api });
    expect(await screen.findByText(BUSY)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Acme Robotics" })).toBeNull();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  });

  test("a failed refetch of the billing accounts keeps the list", async () => {
    const api = createFakeApi();
    renderApp("/billing", { api });
    const list = await screen.findByRole("list", { name: "Billing accounts" });
    await settle();
    api.failNext.set("ListBillingAccounts", BACKGROUND_FAILURE);
    await refocus();
    expect(screen.getByText(BUSY)).toBeTruthy();
    expect(rowTitles(list)).toEqual(["Acme Robotics"]);
    expect(
      rowTitles(screen.getByRole("list", { name: "Billing accounts" })),
    ).toEqual(["Acme Robotics"]);
    await click(screen.getByRole("button", { name: "Try again" }));
    await settle(60);
    expect(screen.queryByText(BUSY)).toBeNull();
    expect(
      rowTitles(screen.getByRole("list", { name: "Billing accounts" })),
    ).toEqual(["Acme Robotics"]);
  });

  test("a failed refetch of a list with more than one page keeps every item", async () => {
    const api = createFakeApi();
    addBillingAccounts(api, 200);
    renderApp("/billing", { api });
    const list = await screen.findByRole("list", { name: "Billing accounts" });
    await settle(100);
    expect(rowTitles(list)).toHaveLength(201);
    api.failNext.set("ListBillingAccounts", BACKGROUND_FAILURE);
    await refocus();
    expect(screen.getByText(BUSY)).toBeTruthy();
    expect(
      rowTitles(screen.getByRole("list", { name: "Billing accounts" })),
    ).toHaveLength(201);
  });

  test("a failed refetch of the members keeps the members", async () => {
    const { api } = await renderAccount();
    await openTab("Members");
    const list = await screen.findByRole("list", { name: "Members" });
    await settle();
    const names = rowTitles(list);
    expect(names.length).toBeGreaterThan(0);
    api.failNext.set("ListMembers", BACKGROUND_FAILURE);
    await refocus();
    expect(screen.getByText(BUSY)).toBeTruthy();
    expect(rowTitles(screen.getByRole("list", { name: "Members" }))).toEqual(
      names,
    );
    await click(screen.getByRole("button", { name: "Try again" }));
    await settle(60);
    expect(screen.queryByText(BUSY)).toBeNull();
    expect(rowTitles(screen.getByRole("list", { name: "Members" }))).toEqual(
      names,
    );
  });

  test("a failed refetch of the balance keeps the balance", async () => {
    const { api } = await renderAccount();
    const balanceBefore = document.querySelector(".balance")?.textContent;
    expect(balanceBefore).toBeTruthy();
    api.failNext.set("GetBalance", BACKGROUND_FAILURE);
    await refocus();
    expect(screen.getAllByText(BUSY).length).toBeGreaterThan(0);
    expect(document.querySelector(".balance")?.textContent).toBe(balanceBefore);
  });

  test("a mutation whose follow-up refetch fails keeps the page and its state", async () => {
    const { api } = await renderAccount(OWNER);
    await openTab("Members");
    await screen.findByRole("list", { name: "Members" });
    await settle();
    // The role change succeeds; the account refetch that follows it fails.
    api.failNext.set("GetBillingAccount", BACKGROUND_FAILURE);
    const role = screen.getByRole("combobox", { name: "Role of Vera Viewer" });
    await click(role);
    await click(await screen.findByRole("option", { name: "Admin" }));
    await settle(100);
    expect(calls(api, "UpdateMemberRole")).toHaveLength(1);
    expect(screen.getByText(BUSY)).toBeTruthy();
    expect(
      screen.getByRole("heading", { name: "Acme Robotics", level: 1 }),
    ).toBeTruthy();
    expect(
      screen
        .getByRole("tab", { name: "Members" })
        .getAttribute("aria-selected"),
    ).toBe("true");
  });
});

describe("continuation of the billing history lists", () => {
  /**
   * The three lists that read on with a button. `seed` gives the account more
   * rows than one page, so that the button shows after the first page; `rows`
   * and `shrink` read and cut the rows of the fake account.
   */
  const lists = [
    {
      name: "invoices",
      method: "ListInvoices",
      tab: "Invoices",
      list: "Invoices",
      more: "Show older invoices",
      // The page size is 200.
      total: 201,
      firstPage: 200,
      seed: (account: ReturnType<typeof accountOf>) =>
        addInvoices(account, 201 - account.invoices.length),
      shrink: (account: ReturnType<typeof accountOf>) => {
        account.invoices.length = 10;
      },
    },
    {
      name: "ledger",
      method: "ListLedgerEntries",
      tab: "Usage",
      list: "Ledger entries",
      more: "Show older entries",
      // The page size is 50.
      total: 61,
      firstPage: 50,
      seed: (account: ReturnType<typeof accountOf>) =>
        addLedgerEntries(account, 61 - account.ledger.length),
      shrink: (account: ReturnType<typeof accountOf>) => {
        account.ledger.length = 10;
      },
    },
    {
      name: "usage summary",
      method: "ListUsageSummary",
      tab: "Usage",
      list: undefined,
      more: "Show more usage",
      total: 201,
      firstPage: 200,
      seed: (account: ReturnType<typeof accountOf>) =>
        addUsageRows(account, 201 - account.usage.length),
      shrink: (account: ReturnType<typeof accountOf>) => {
        account.usage.length = 10;
      },
    },
  ];

  function accountOf(api: ReturnType<typeof createFakeApi>) {
    return api.accounts.get(ACCOUNT_ID)!;
  }

  /** The rendered rows of the list: list items, or usage table rows. */
  function rowCount(list: string | undefined) {
    return list === undefined
      ? within(screen.getByRole("table")).getAllByRole("row").length - 1
      : within(screen.getByRole("list", { name: list })).getAllByRole(
          "listitem",
        ).length;
  }

  /** Opens the list with its first page loaded. */
  async function openList(entry: (typeof lists)[number]) {
    const api = createFakeApi();
    entry.seed(accountOf(api));
    await renderAccount(OWNER, { api });
    await openTab(entry.tab);
    await screen.findByRole("button", { name: entry.more });
    await settle(60);
    expect(rowCount(entry.list)).toBe(entry.firstPage);
    expect(pageTokens(api, entry.method)).toEqual([""]);
    return api;
  }

  const moreButton = (entry: (typeof lists)[number]) =>
    screen.getByRole("button", { name: entry.more }) as HTMLButtonElement;

  for (const entry of lists) {
    describe(entry.name, () => {
      test("reads on with the next token, appends the rows once and ends with an empty token", async () => {
        const api = await openList(entry);
        expect(moreButton(entry).disabled).toBe(false);
        await click(moreButton(entry));
        await settle(60);
        expect(pageTokens(api, entry.method)).toEqual(["", nonemptyPageToken]);
        expect(rowCount(entry.list)).toBe(entry.total);
        expect(screen.queryByRole("button", { name: entry.more })).toBeNull();
      });

      test("a continuation during a pending refresh neither cancels it nor uses the cached token", async () => {
        const api = await openList(entry);
        let release!: () => void;
        api.gates.set(
          entry.method,
          new Promise<void>((resolve) => {
            release = resolve;
          }),
        );
        await refocus();
        // The refresh holds the first page.
        expect(pageTokens(api, entry.method)).toEqual(["", ""]);
        expect(moreButton(entry).disabled).toBe(true);
        await click(moreButton(entry));
        expect(pageTokens(api, entry.method)).toEqual(["", ""]);
        release();
        api.gates.delete(entry.method);
        await settle(60);
        // The refresh finished and no request used the cached token.
        expect(pageTokens(api, entry.method)).toEqual(["", ""]);
        expect(rowCount(entry.list)).toBe(entry.firstPage);
        expect(moreButton(entry).disabled).toBe(false);
        await click(moreButton(entry));
        await settle(60);
        expect(pageTokens(api, entry.method)).toEqual([
          "",
          "",
          nonemptyPageToken,
        ]);
        expect(rowCount(entry.list)).toBe(entry.total);
      });

      test("after a failed refresh the rows and the retry stay and the continuation waits for the retry", async () => {
        const api = await openList(entry);
        api.failNext.set(entry.method, BACKGROUND_FAILURE);
        await refocus();
        expect(screen.getByText(BUSY)).toBeTruthy();
        expect(rowCount(entry.list)).toBe(entry.firstPage);
        expect(moreButton(entry).disabled).toBe(true);
        await click(moreButton(entry));
        expect(pageTokens(api, entry.method)).toEqual(["", ""]);

        // A successful retry supplies the token again.
        await click(screen.getByRole("button", { name: "Try again" }));
        await settle(60);
        expect(screen.queryByText(BUSY)).toBeNull();
        expect(pageTokens(api, entry.method)).toEqual(["", "", ""]);
        expect(moreButton(entry).disabled).toBe(false);
        await click(moreButton(entry));
        await settle(60);
        expect(pageTokens(api, entry.method)).toEqual([
          "",
          "",
          "",
          nonemptyPageToken,
        ]);
        expect(rowCount(entry.list)).toBe(entry.total);
      });

      test("a retry that returns an empty token removes the continuation", async () => {
        const api = await openList(entry);
        api.failNext.set(entry.method, BACKGROUND_FAILURE);
        await refocus();
        expect(moreButton(entry).disabled).toBe(true);
        // The list shrank to one page while the refresh failed.
        entry.shrink(accountOf(api));
        await click(screen.getByRole("button", { name: "Try again" }));
        await settle(60);
        expect(screen.queryByRole("button", { name: entry.more })).toBeNull();
        expect(pageTokens(api, entry.method)).toEqual(["", "", ""]);
      });
    });
  }
});
