// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { CreditService } from "@delino/accounts-protos/v1/credit_pb";
import { BillingUsageService } from "@delino/accounts-protos/v1/usage_pb";
import {
  ACCOUNT_ID,
  addInvoices,
  addLedgerEntries,
  addUsageRows,
  ADMIN,
  createFakeApi,
  VIEWER,
} from "./testing/fake-api.ts";

/** Seeds 201 rows per list, so that the first page of 200 leaves one row. */
function seeded() {
  const api = createFakeApi();
  const account = api.accounts.get(ACCOUNT_ID)!;
  account.ledger = [];
  account.invoices = [];
  account.usage = [];
  addLedgerEntries(account, 201);
  addInvoices(account, 201);
  addUsageRows(account, 201);
  return {
    api,
    credit: createClient(CreditService, api.transport),
    usage: createClient(BillingUsageService, api.transport),
  };
}

const NOW = new Date();
const MONTH = { year: NOW.getUTCFullYear(), month: NOW.getUTCMonth() + 1 };

const rejection = async (promise: Promise<unknown>) => {
  try {
    await promise;
  } catch (error) {
    return ConnectError.from(error);
  }
  throw new Error("the call did not fail");
};

describe("fake billing history pagination", () => {
  test("ListLedgerEntries pages by the page size and ends with an empty token", async () => {
    const { credit } = seeded();
    const first = await credit.listLedgerEntries({
      billingAccountId: ACCOUNT_ID,
      pageSize: 200,
    });
    expect(first.ledgerEntries).toHaveLength(200);
    expect(first.nextPageToken).not.toBe("");
    const last = await credit.listLedgerEntries({
      billingAccountId: ACCOUNT_ID,
      pageSize: 200,
      pageToken: first.nextPageToken,
    });
    expect(last.ledgerEntries.map((entry) => entry.organizationId)).toEqual([
      "org-0201",
    ]);
    expect(last.nextPageToken).toBe("");
  });

  test("ListInvoices pages by the page size and ends with an empty token", async () => {
    const { credit } = seeded();
    const first = await credit.listInvoices({
      billingAccountId: ACCOUNT_ID,
      pageSize: 200,
    });
    expect(first.invoices).toHaveLength(200);
    expect(first.nextPageToken).not.toBe("");
    const last = await credit.listInvoices({
      billingAccountId: ACCOUNT_ID,
      pageSize: 200,
      pageToken: first.nextPageToken,
    });
    expect(last.invoices.map((invoice) => invoice.paddleTransactionId)).toEqual(
      ["txn_inv0201"],
    );
    expect(last.nextPageToken).toBe("");
  });

  test("ListUsageSummary pages the rows and reports the month total on every page", async () => {
    const { usage } = seeded();
    const first = await usage.listUsageSummary({
      billingAccountId: ACCOUNT_ID,
      ...MONTH,
      pageSize: 200,
    });
    expect(first.rows).toHaveLength(200);
    expect(first.nextPageToken).not.toBe("");
    const last = await usage.listUsageSummary({
      billingAccountId: ACCOUNT_ID,
      ...MONTH,
      pageSize: 200,
      pageToken: first.nextPageToken,
    });
    expect(last.rows.map((row) => row.organizationId)).toEqual([
      "usage-org-0201",
    ]);
    expect(last.nextPageToken).toBe("");
    expect(first.totalAmountUsdMicros).toBe(201n);
    expect(last.totalAmountUsdMicros).toBe(201n);
  });

  test("an omitted page size is 50 and a larger one is capped at 200", async () => {
    const { credit } = seeded();
    const byDefault = await credit.listLedgerEntries({
      billingAccountId: ACCOUNT_ID,
    });
    expect(byDefault.ledgerEntries).toHaveLength(50);
    expect(byDefault.nextPageToken).not.toBe("");
    const capped = await credit.listInvoices({
      billingAccountId: ACCOUNT_ID,
      pageSize: 1000,
    });
    expect(capped.invoices).toHaveLength(200);
  });

  test("a list that fits one page has an empty token", async () => {
    const api = createFakeApi();
    const credit = createClient(CreditService, api.transport);
    const res = await credit.listLedgerEntries({
      billingAccountId: ACCOUNT_ID,
      pageSize: 200,
    });
    expect(res.ledgerEntries).toHaveLength(2);
    expect(res.nextPageToken).toBe("");
  });

  test("an invalid page token is rejected with InvalidArgument", async () => {
    const { credit, usage } = seeded();
    const calls = [
      credit.listLedgerEntries({
        billingAccountId: ACCOUNT_ID,
        pageToken: "bogus",
      }),
      credit.listInvoices({ billingAccountId: ACCOUNT_ID, pageToken: "bogus" }),
      usage.listUsageSummary({
        billingAccountId: ACCOUNT_ID,
        ...MONTH,
        pageToken: "bogus",
      }),
    ];
    for (const call of calls) {
      expect((await rejection(call)).code).toBe(Code.InvalidArgument);
    }
  });

  test("the role checks still apply to every page", async () => {
    const { api, credit, usage } = seeded();
    api.userId = ADMIN;
    expect(
      (await credit.listInvoices({ billingAccountId: ACCOUNT_ID })).invoices,
    ).toHaveLength(50);
    api.userId = VIEWER;
    expect(
      (
        await rejection(
          credit.listInvoices({
            billingAccountId: ACCOUNT_ID,
            pageToken: "p1",
          }),
        )
      ).code,
    ).toBe(Code.PermissionDenied);
    // Every member reads the ledger and the usage summary.
    expect(
      (await credit.listLedgerEntries({ billingAccountId: ACCOUNT_ID }))
        .ledgerEntries,
    ).toHaveLength(50);
    expect(
      (await usage.listUsageSummary({ billingAccountId: ACCOUNT_ID, ...MONTH }))
        .rows,
    ).toHaveLength(50);
  });
});
