// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, rs, test } from "@rstest/core";
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
  OUTSIDER,
  VIEWER,
} from "./testing/fake-api.ts";

const DECEMBER = { year: 2026, month: 12 };
const JANUARY = { year: 2027, month: 1 };
const UTC_ROLLOVER = Date.parse("2027-01-01T00:00:00.000Z");

/** Seeds 201 rows per list, so that the first page of 200 leaves one row. */
function seeded() {
  const api = createFakeApi({ usageMonth: DECEMBER });
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

const rejection = async (promise: Promise<unknown>) => {
  try {
    await promise;
  } catch (error) {
    return ConnectError.from(error);
  }
  throw new Error("the call did not fail");
};

describe("fake billing history pagination", () => {
  afterEach(() => {
    rs.useRealTimers();
  });

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

  test("ListLedgerEntries continues at the next row when the page size changes", async () => {
    const api = createFakeApi();
    const account = api.accounts.get(ACCOUNT_ID)!;
    account.ledger = [];
    addLedgerEntries(account, 6);
    const credit = createClient(CreditService, api.transport);
    const request = { billingAccountId: ACCOUNT_ID };

    const first = await credit.listLedgerEntries({ ...request, pageSize: 2 });
    expect(first.ledgerEntries.map((entry) => entry.organizationId)).toEqual([
      "org-0001",
      "org-0002",
    ]);
    expect(first.nextPageToken).not.toBe("");

    const continuation = { ...request, pageToken: first.nextPageToken };
    const larger = await credit.listLedgerEntries({
      ...continuation,
      pageSize: 3,
    });
    expect(larger.ledgerEntries.map((entry) => entry.organizationId)).toEqual([
      "org-0003",
      "org-0004",
      "org-0005",
    ]);
    expect(larger.nextPageToken).not.toBe("");

    const last = await credit.listLedgerEntries({
      ...request,
      pageSize: 1,
      pageToken: larger.nextPageToken,
    });
    expect(last.ledgerEntries.map((entry) => entry.organizationId)).toEqual([
      "org-0006",
    ]);
    expect(last.nextPageToken).toBe("");

    const smaller = await credit.listLedgerEntries({
      ...continuation,
      pageSize: 1,
    });
    expect(smaller.ledgerEntries.map((entry) => entry.organizationId)).toEqual([
      "org-0003",
    ]);
    expect(smaller.nextPageToken).not.toBe("");

    const same = await credit.listLedgerEntries({
      ...continuation,
      pageSize: 2,
    });
    expect(same.ledgerEntries.map((entry) => entry.organizationId)).toEqual([
      "org-0003",
      "org-0004",
    ]);
    expect(same.nextPageToken).not.toBe("");
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

  test("ListUsageSummary keeps the configured month's rows and total across UTC rollover", async () => {
    rs.useFakeTimers({ now: UTC_ROLLOVER - 1, toFake: ["Date"] });
    const { usage } = seeded();
    const request = {
      billingAccountId: ACCOUNT_ID,
      ...DECEMBER,
      pageSize: 200,
    };
    const first = await usage.listUsageSummary(request);
    expect(first.rows).toHaveLength(200);
    expect(first.nextPageToken).not.toBe("");
    expect(first.totalAmountUsdMicros).toBe(201n);

    rs.setSystemTime(UTC_ROLLOVER);
    const last = await usage.listUsageSummary({
      ...request,
      pageToken: first.nextPageToken,
    });
    expect(last.rows.map((row) => row.organizationId)).toEqual([
      "usage-org-0201",
    ]);
    expect(last.nextPageToken).toBe("");
    expect(last.totalAmountUsdMicros).toBe(201n);

    const fresh = await usage.listUsageSummary(request);
    expect(fresh).toEqual(first);
  });

  test("ListUsageSummary uses the current UTC month on each request by default", async () => {
    rs.useFakeTimers({ now: UTC_ROLLOVER - 1, toFake: ["Date"] });
    const { transport } = createFakeApi();
    const usage = createClient(BillingUsageService, transport);
    const before = await usage.listUsageSummary({
      billingAccountId: ACCOUNT_ID,
      ...DECEMBER,
    });
    expect(before.rows.length).toBeGreaterThan(0);

    rs.setSystemTime(UTC_ROLLOVER);
    const previous = await usage.listUsageSummary({
      billingAccountId: ACCOUNT_ID,
      ...DECEMBER,
    });
    expect(previous.rows).toEqual([]);
    expect(previous.totalAmountUsdMicros).toBe(0n);
    expect(previous.nextPageToken).toBe("");
    const current = await usage.listUsageSummary({
      billingAccountId: ACCOUNT_ID,
      ...JANUARY,
    });
    expect(current).toEqual(before);
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
    const { credit } = seeded();
    const calls = [
      credit.listLedgerEntries({
        billingAccountId: ACCOUNT_ID,
        pageToken: "bogus",
      }),
      credit.listInvoices({ billingAccountId: ACCOUNT_ID, pageToken: "bogus" }),
    ];
    for (const call of calls) {
      expect((await rejection(call)).code).toBe(Code.InvalidArgument);
    }
  });

  test("the role checks still apply to every page", async () => {
    const { api, credit } = seeded();
    api.userId = ADMIN;
    const first = await credit.listInvoices({ billingAccountId: ACCOUNT_ID });
    expect(first.invoices).toHaveLength(50);
    expect(first.nextPageToken).not.toBe("");
    api.userId = VIEWER;
    expect(
      (
        await rejection(
          credit.listInvoices({
            billingAccountId: ACCOUNT_ID,
            pageToken: first.nextPageToken,
          }),
        )
      ).code,
    ).toBe(Code.PermissionDenied);
    expect(
      (await credit.listLedgerEntries({ billingAccountId: ACCOUNT_ID }))
        .ledgerEntries,
    ).toHaveLength(50);
  });

  test("ListUsageSummary keeps member access and token validation after rollover", async () => {
    rs.useFakeTimers({ now: UTC_ROLLOVER, toFake: ["Date"] });
    const { api, usage } = seeded();
    api.userId = VIEWER;
    const request = { billingAccountId: ACCOUNT_ID, ...DECEMBER };
    const first = await usage.listUsageSummary(request);
    expect(first.rows).toHaveLength(50);
    expect(first.nextPageToken).not.toBe("");

    const otherMonth = await usage.listUsageSummary({
      billingAccountId: ACCOUNT_ID,
      ...JANUARY,
    });
    expect(otherMonth.rows).toEqual([]);
    expect(otherMonth.totalAmountUsdMicros).toBe(0n);
    expect(otherMonth.nextPageToken).toBe("");

    expect(
      (
        await rejection(
          usage.listUsageSummary({ ...request, pageToken: "bogus" }),
        )
      ).code,
    ).toBe(Code.InvalidArgument);

    api.userId = OUTSIDER;
    const denied = await rejection(
      usage.listUsageSummary({ ...request, pageToken: first.nextPageToken }),
    );
    expect(denied.code).toBe(Code.NotFound);
  });
});
