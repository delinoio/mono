// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  GetBillingAccountResponseSchema,
  ListBillingAccountsResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import {
  GetInvoiceUrlResponseSchema,
  LedgerEntryKind,
  ListInvoicesResponseSchema,
} from "@delino/accounts-protos/v1/credit_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

const INVOICE_PATH = "/fixture-invoice.pdf";
const ACCOUNT_ID = "0193a1b2-0000-7000-8000-000000000001";
const ACCOUNT_PATH = `/billing/${ACCOUNT_ID}`;
const ACCOUNT_NAME = "Example billing account";

async function installInvoiceFixture(
  browser: Browser,
  appOrigin: string,
  popupBlocked = false,
) {
  await seedFixtureSession(browser, appOrigin);
  if (popupBlocked) {
    await browser.addInitScript(() => {
      window.open = () => null;
    });
  }

  let markStarted!: () => void;
  const started = new Promise<void>((resolve) => {
    markStarted = resolve;
  });
  let release!: () => void;
  const responseGate = new Promise<void>((resolve) => {
    release = resolve;
  });
  const account = {
    billingAccountId: ACCOUNT_ID,
    name: ACCOUNT_NAME,
    callerRole: BillingAccountRole.OWNER,
  };
  const responses = new Map([
    [
      "ListBillingAccounts",
      toBinary(
        ListBillingAccountsResponseSchema,
        create(ListBillingAccountsResponseSchema, {
          billingAccounts: [account],
        }),
      ),
    ],
    [
      "ListInvoices",
      toBinary(
        ListInvoicesResponseSchema,
        create(ListInvoicesResponseSchema, {
          invoices: [
            {
              paddleTransactionId: "txn_fixture",
              amountUsdMicros: 25_000_000n,
              kind: LedgerEntryKind.MANUAL_TOP_UP,
            },
          ],
        }),
      ),
    ],
    [
      "GetInvoiceUrl",
      toBinary(
        GetInvoiceUrlResponseSchema,
        create(GetInvoiceUrlResponseSchema, {
          invoiceUrl: API_ORIGIN + INVOICE_PATH,
        }),
      ),
    ],
  ]);

  await browser.route(`${API_ORIGIN}/**`, async (route) => {
    const path = new URL(route.request.url).pathname;
    if (path === INVOICE_PATH) {
      await route.fulfill({
        contentType: "text/html",
        body: "<title>Fixture invoice</title><h1>Fixture invoice</h1>",
      });
      return;
    }
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }
    const method = path.split("/").pop()!;
    if (method === "GetInvoiceUrl") {
      markStarted();
      await responseGate;
    }
    const body =
      method === "GetBillingAccount"
        ? toBinary(
            GetBillingAccountResponseSchema,
            create(GetBillingAccountResponseSchema, {
              billingAccount: account,
            }),
          )
        : responses.get(method);
    await route.fulfill({
      contentType: "application/proto",
      headers,
      // An empty protobuf response is enough for the Overview reads.
      body: body ?? new Uint8Array(),
    });
  });
  return {
    async refetchAs(role: BillingAccountRole) {
      account.callerRole = role;
      await browser.evaluate(async () => {
        const now = Date.now;
        // Make the cached account stale for the window focus refetch.
        Date.now = () => now() + 11_000;
        try {
          window.dispatchEvent(new Event("visibilitychange"));
          // React Query checks staleness after resuming paused mutations.
          await Promise.resolve();
        } finally {
          Date.now = now;
        }
        return null;
      });
    },
    started,
    async respond() {
      const response = browser.waitForResponse(/\/GetInvoiceUrl$/);
      release();
      return response;
    },
    release,
  };
}

describe("invoice access", () => {
  test("keeps Overview selected when access returns on the open account", async ({
    app,
    browser,
    screen,
  }) => {
    const invoice = await installInvoiceFixture(browser, app.baseUrl);
    await app.open(ACCOUNT_PATH);
    const invoices = screen.getByRole("tab", "Invoices");
    const invoicePanel = screen.getByRole("tabpanel", "Invoices");
    const overviewPanel = screen.getByRole("tabpanel", "Overview");
    const selectedTab = browser.locator('[role="tab"][aria-selected="true"]');
    await invoices.click();

    await invoice.refetchAs(BillingAccountRole.ADMIN);
    await expect(screen.getByText("Your role: Admin")).toBeVisible();
    await expect(selectedTab).toHaveText("Invoices");
    await expect(invoicePanel).toBeVisible();
    await app.screenshot("invoice-access-retained");

    await invoice.refetchAs(BillingAccountRole.VIEWER);
    await expect(screen.getByText("Your role: Viewer")).toBeVisible();
    await expect(invoices).toBeHidden();
    await expect(invoicePanel).toBeHidden();
    await expect(selectedTab).toHaveText("Overview");
    await expect(overviewPanel).toBeVisible();
    await app.screenshot("invoice-access-removed");

    await invoice.refetchAs(BillingAccountRole.ADMIN);
    await expect(screen.getByText("Your role: Admin")).toBeVisible();
    await expect(invoices).toBeVisible();
    await expect(invoices).toHaveAttribute("aria-selected", "false");
    await expect(selectedTab).toHaveText("Overview");
    await expect(overviewPanel).toBeVisible();
    await expect(browser).toHaveURL(app.baseUrl + ACCOUNT_PATH);
    await app.screenshot("invoice-access-restored");

    await invoices.click();
    await expect(selectedTab).toHaveText("Invoices");
    await expect(invoicePanel).toBeVisible();
    await app.screenshot("invoices-reselected");
  });
});

describe("invoice navigation", () => {
  for (const reopen of [false, true]) {
    test(
      reopen
        ? "keeps the reopened Invoices panel after a blocked pop-up"
        : "keeps the billing list after a blocked pop-up",
      async ({ app, browser, screen }) => {
        const invoice = await installInvoiceFixture(browser, app.baseUrl, true);
        try {
          await app.open(ACCOUNT_PATH);
          await screen.getByRole("tab", "Invoices").click();
          await screen.getByRole("button", /Open the invoice/).click();
          await invoice.started;
          await screen.getByRole("link", "Billing accounts").click();
          await expect(
            screen.getByRole("heading", "Billing accounts", { level: 1 }),
          ).toBeVisible();
          if (reopen) {
            await screen.getByRole("link", new RegExp(ACCOUNT_NAME)).click();
            await screen.getByRole("tab", "Invoices").click();
          }

          await (await invoice.respond()).body();
          // Let the page act on the completed response before checking the URL.
          await browser.evaluate(
            () =>
              new Promise<number>((resolve) => {
                requestAnimationFrame(() => requestAnimationFrame(resolve));
              }),
          );
          await expect(browser).toHaveURL(
            app.baseUrl + (reopen ? ACCOUNT_PATH : "/billing"),
          );
          await expect(
            screen.getByRole(
              "heading",
              reopen ? "Invoices" : "Billing accounts",
            ),
          ).toBeVisible();
          await app.screenshot(reopen ? "reopened-invoices" : "billing-list");
        } finally {
          invoice.release();
        }
      },
    );
  }

  test("uses the current tab when a pop-up is blocked and Invoices stays open", async ({
    app,
    browser,
    screen,
  }) => {
    const invoice = await installInvoiceFixture(browser, app.baseUrl, true);
    try {
      await app.open(ACCOUNT_PATH);
      await screen.getByRole("tab", "Invoices").click();
      await screen.getByRole("button", /Open the invoice/).click();
      await invoice.started;

      await invoice.respond();

      await expect(
        screen.getByRole("heading", "Fixture invoice"),
      ).toBeVisible();
      await expect(browser).toHaveURL(API_ORIGIN + INVOICE_PATH);
    } finally {
      invoice.release();
    }
  });

  for (const closePopup of [false, true]) {
    test(
      closePopup
        ? "discards the link when the user closes the invoice pop-up"
        : "loads the link in an open pop-up after leaving Invoices",
      async ({ app, browser, screen }) => {
        const invoice = await installInvoiceFixture(browser, app.baseUrl);
        try {
          await app.open(ACCOUNT_PATH);
          await screen.getByRole("tab", "Invoices").click();
          const [billingTab] = browser.tabs();
          const popup = await browser.waitForPopup(() =>
            screen.getByRole("button", /Open the invoice/).click(),
          );
          await invoice.started;
          await billingTab!.activate();
          await screen.getByRole("link", "Billing accounts").click();
          if (closePopup) await popup.close();

          await invoice.respond();

          await expect(
            screen.getByRole("heading", "Billing accounts", { level: 1 }),
          ).toBeVisible();
          if (closePopup) {
            expect(browser.tabs()).toHaveLength(1);
          } else {
            await popup.activate();
            await expect(
              screen.getByRole("heading", "Fixture invoice"),
            ).toBeVisible();
          }
        } finally {
          invoice.release();
        }
      },
    );
  }
});
