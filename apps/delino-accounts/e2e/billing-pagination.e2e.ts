// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  ListBillingAccountsRequestSchema,
  ListBillingAccountsResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

const tokens = ["", "p1", "p2", "p3"];
const names = ["Acme", "Bravo", "Cedar", "Delta"];
const requestBodies = tokens.map((pageToken) =>
  Buffer.from(
    toBinary(
      ListBillingAccountsRequestSchema,
      create(ListBillingAccountsRequestSchema, { pageSize: 200, pageToken }),
    ),
  ).toString("utf8"),
);

async function serveBillingPages(
  browser: Browser,
  appOrigin: string,
  pageCount: number,
  {
    failPage,
    firstPageSize = 1,
  }: { failPage?: number; firstPageSize?: number } = {},
) {
  await seedFixtureSession(browser, appOrigin);
  const requestedTokens: string[] = [];
  let firstPageRequests = 0;
  await browser.route(`${API_ORIGIN}/**/ListBillingAccounts`, async (route) => {
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }

    // The route API exposes the binary request as a UTF-8 string.
    const page = requestBodies.indexOf(route.request.postData ?? "");
    if (page < 0 || page >= pageCount) {
      throw new Error("Unexpected ListBillingAccounts request");
    }
    requestedTokens.push(tokens[page]);
    if (page === 0) firstPageRequests += 1;
    if (page === failPage) {
      failPage = undefined;
      await route.fulfill({
        status: 429,
        contentType: "application/json",
        headers,
        body: '{"code":"resource_exhausted"}',
      });
      return;
    }
    const response = create(ListBillingAccountsResponseSchema, {
      billingAccounts: Array.from(
        { length: page === 0 ? firstPageSize : 1 },
        (_, index) => {
          const id = String(page * 200 + index + 1).padStart(12, "0");
          const name = index === 0 ? names[page] : `Account ${index + 1}`;
          return {
            billingAccountId: `0193a1b2-0000-7000-8000-${id}`,
            name:
              firstPageRequests === 1
                ? name
                : `${name} refresh ${firstPageRequests - 1}`,
            callerRole: BillingAccountRole.OWNER,
          };
        },
      ),
      nextPageToken: page + 1 < pageCount ? tokens[page + 1] : "",
    });
    await route.fulfill({
      contentType: "application/proto",
      headers,
      body: toBinary(ListBillingAccountsResponseSchema, response),
    });
  });
  return requestedTokens;
}

describe("billing account pagination", () => {
  test("reads four immediate pages in order", async ({
    app,
    browser,
    screen,
  }) => {
    const requestedTokens = await serveBillingPages(browser, app.baseUrl, 4);
    await app.open("/billing");

    const rows = screen
      .getByRole("list", "Billing accounts")
      .getByRole("listitem");
    await expect(rows).toHaveCount(4);
    for (const [index, name] of names.entries()) {
      await expect(
        rows.nth(index).getByRole("link", `${name} Owner`),
      ).toBeVisible();
    }
    await expect(screen.getByText("Loading more")).toBeHidden();
    expect(requestedTokens).toEqual(tokens);
    await app.screenshot("billing-all-pages");
  });

  test("stops when the first page has no continuation token", async ({
    app,
    browser,
    screen,
  }) => {
    const requestedTokens = await serveBillingPages(browser, app.baseUrl, 1);
    await app.open("/billing");

    await expect(screen.getByRole("link", "Acme Owner")).toBeVisible();
    await expect(screen.getByText("Loading more")).toBeHidden();
    expect(requestedTokens).toEqual([""]);
    await app.screenshot("billing-first-page-terminal");
  });

  test("keeps loaded rows and retries the failed token", async ({
    app,
    browser,
    screen,
  }) => {
    const requestedTokens = await serveBillingPages(browser, app.baseUrl, 4, {
      failPage: 2,
    });
    await app.open("/billing");

    const rows = screen
      .getByRole("list", "Billing accounts")
      .getByRole("listitem");
    const errorMessage = screen.getByText(/Some items did not load/);
    const loading = screen.getByText("Loading more");
    await expect(errorMessage).toBeVisible();
    await expect(rows).toHaveCount(2);
    await expect(loading).toBeHidden();
    expect(requestedTokens).toEqual(["", "p1", "p2"]);
    await app.screenshot("billing-continuation-paused");

    await screen.getByRole("button", "Try again").click();
    await expect(rows).toHaveCount(4);
    await expect(errorMessage).toBeHidden();
    await expect(loading).toBeHidden();
    expect(requestedTokens).toEqual(["", "p1", "p2", "p2", "p3"]);
    await app.screenshot("billing-continuation-recovered");
  });
});

// Age the cache past the app's 10-second stale time without waiting 10 seconds.
async function ageCache(browser: Browser) {
  await browser.evaluate(() => {
    const now = Date.now;
    Date.now = () => now() + 11_000;
    return null;
  });
}

async function reconnect(browser: Browser) {
  await browser.evaluate(() => {
    window.dispatchEvent(new Event("offline"));
    window.dispatchEvent(new Event("online"));
    return null;
  });
}

for (const event of ["reconnect", "remount"] as const) {
  test(`a failed continuation waits for retry after ${event}`, async ({
    app,
    browser,
    screen,
  }) => {
    const requestedTokens = await serveBillingPages(browser, app.baseUrl, 2, {
      failPage: 1,
      firstPageSize: 200,
    });
    await app.open("/billing");

    const rows = screen
      .getByRole("list", "Billing accounts")
      .getByRole("listitem");
    const notice = screen.getByText(/Some items did not load/);
    await expect(notice).toBeVisible();
    await expect(rows).toHaveCount(200);
    expect(requestedTokens).toEqual(["", "p1"]);

    await ageCache(browser);
    if (event === "reconnect") {
      await reconnect(browser);
    } else {
      await screen.getByRole("link", "Delino Accounts").click();
      await expect(rows).toHaveCount(0);
      await screen.getByRole("link", "Manage billing").click();
    }

    await expect(notice).toBeVisible();
    await expect(rows).toHaveCount(200);
    await app.screenshot(`billing-continuation-after-${event}`);
    expect(requestedTokens).toEqual(["", "p1"]);
    await screen.getByRole("button", "Try again").click();
    await expect(rows).toHaveCount(201);
    await expect(notice).toBeHidden();
    expect(requestedTokens).toEqual(["", "p1", "p1"]);
    await app.screenshot(`billing-continuation-recovered-after-${event}`);
  });
}

test("healthy stale lists refresh after reconnect and remount", async ({
  app,
  browser,
  screen,
}) => {
  const requestedTokens = await serveBillingPages(browser, app.baseUrl, 2, {
    firstPageSize: 200,
  });
  await app.open("/billing");

  const rows = screen
    .getByRole("list", "Billing accounts")
    .getByRole("listitem");
  await expect(rows).toHaveCount(201);
  expect(requestedTokens).toEqual(["", "p1"]);

  await ageCache(browser);
  await reconnect(browser);
  await expect(screen.getByRole("link", "Bravo refresh 1 Owner")).toBeVisible();
  expect(requestedTokens).toEqual(["", "p1", "", "p1"]);

  await ageCache(browser);
  await screen.getByRole("link", "Delino Accounts").click();
  await expect(rows).toHaveCount(0);
  await screen.getByRole("link", "Manage billing").click();
  await expect(screen.getByRole("link", "Bravo refresh 2 Owner")).toBeVisible();
  await expect(rows).toHaveCount(201);
  expect(requestedTokens).toEqual(["", "p1", "", "p1", "", "p1"]);
});
