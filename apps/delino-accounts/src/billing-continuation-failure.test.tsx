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
import {
  ACCOUNT_ID,
  addInvoices,
  addLedgerEntries,
  addUsageRows,
  createFakeApi,
  numbered,
  OWNER,
} from "./testing/fake-api.ts";
import { listRows } from "./testing/list-rows.ts";
import { renderApp } from "./testing/render.tsx";

/**
 * A failed request for a later page of the invoices, the usage summary and
 * the ledger. These lists read on with a button, so the failure of that
 * request must keep the loaded rows and show a notice that retries the failed
 * page.
 */

type FakeApi = ReturnType<typeof createFakeApi>;

const ACCOUNT_PATH = `/billing/${ACCOUNT_ID}`;
const UNAVAILABLE =
  "Delino Accounts is not available right now. Try again in a moment.";
const SIGN_IN = "Sign in with Delino";
const TRY_AGAIN = "Try again";
const USAGE_MONTH = { year: 2026, month: 12 };
const UTC_ROLLOVER = Date.parse("2027-01-01T00:00:00Z");

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  rs.useRealTimers();
});

async function click(element: Element) {
  await act(async () => {
    fireEvent.click(element);
  });
}

function accountOf(api: FakeApi) {
  return api.accounts.get(ACCOUNT_ID)!;
}

function pageTokens(api: FakeApi, method: string) {
  return api.calls
    .filter((c) => c.method === method)
    .map((c) => c.request.pageToken);
}

/**
 * The rendered rows of the list: list items, or usage table rows.
 *
 * The rows are counted from the DOM instead of with `getAllByRole`. A role
 * query computes the accessible role of every element of a list of 200 rows,
 * which costs tens of milliseconds, and `waitFor` and `findBy*` repeat it on
 * every DOM mutation while the lists render. On a loaded CI runner that made
 * the tests exceed the 5 s test timeout. See `testing/list-rows.ts`; the
 * usage table rows are scoped to the body of the usage table.
 */
function usageRows() {
  const body = document.querySelector(
    '[aria-labelledby="usage-table-caption"] > table > tbody',
  );
  if (body === null) throw new Error("no usage table body");
  return Array.from(body.querySelectorAll<HTMLTableRowElement>(":scope > tr"));
}

function rowCount(list: string | undefined) {
  return list === undefined ? usageRows().length : listRows(list).length;
}

/**
 * The "read on" button, found by its text. `findByRole` with a name would
 * compute the role of every element of the 200 rows on each DOM mutation.
 */
function moreButton(more: string) {
  return screen.queryByText(more, {
    selector: "button",
  }) as HTMLButtonElement | null;
}

/**
 * The three lists that read on with a button. `seed` gives the account more
 * rows than one page, so that the button shows after the first page.
 */
const lists = [
  {
    name: "invoices",
    method: "ListInvoices",
    tab: "Invoices",
    list: "Invoices",
    more: "Show older invoices",
    firstPage: 200,
    readRowIdentities() {
      return listRows(this.list).map(
        (row) => row.querySelector("code")?.textContent,
      );
    },
    seed(account: ReturnType<typeof accountOf>) {
      addInvoices(account, 201 - account.invoices.length);
      return account.invoices.map((invoice) => invoice.paddleTransactionId);
    },
  },
  {
    name: "ledger",
    method: "ListLedgerEntries",
    tab: "Usage",
    list: "Ledger entries",
    more: "Show older entries",
    firstPage: 50,
    readRowIdentities() {
      return listRows(this.list).map((row) =>
        row
          .querySelector(".list-main .muted")
          ?.textContent?.split(" · ")
          .slice(1)
          .join(" · "),
      );
    },
    seed(account: ReturnType<typeof accountOf>) {
      const generatedCount = 61 - account.ledger.length;
      addLedgerEntries(account, generatedCount);
      // Newest first. The generated Nov 2 entries share a timestamp, so
      // their IDs put them in descending order between the two seeded entries.
      return [
        "OwnSand · org-1 · Relay egress · 1,048,576 bytes",
        ...Array.from(
          { length: generatedCount },
          (_, i) =>
            `OwnSand · org-${numbered(generatedCount - i)} · Relay egress · 1 byte`,
        ),
        "Paddle txn_sub1",
      ];
    },
  },
  {
    name: "usage summary",
    method: "ListUsageSummary",
    tab: "Usage",
    list: undefined,
    more: "Show more usage",
    firstPage: 200,
    readRowIdentities() {
      return usageRows().map((row) => [
        row.cells[1]?.textContent,
        row.cells[2]?.textContent,
      ]);
    },
    seed(account: ReturnType<typeof accountOf>) {
      account.usage.length = 0;
      addUsageRows(account, 201, 1_000_000n);
      return account.usage.map((row) => [row.organizationId, "Relay egress"]);
    },
  },
];

/** Opens the list with its first page loaded and `failure` armed. */
async function openList(
  entry: (typeof lists)[number],
  failure: ConnectError | undefined,
  { rolloverUsageMonth = false } = {},
) {
  const usageMonth =
    entry.method === "ListUsageSummary" ? USAGE_MONTH : undefined;
  if (usageMonth !== undefined) {
    rs.useFakeTimers({ now: UTC_ROLLOVER - 1_000, toFake: ["Date"] });
  }
  const api = createFakeApi({ usageMonth });
  api.userId = OWNER;
  const expectedRows = entry.seed(accountOf(api));
  let releaseUsageResponse!: () => void;
  if (rolloverUsageMonth) {
    api.gates.set(
      entry.method,
      new Promise<void>((resolve) => {
        releaseUsageResponse = resolve;
      }),
    );
  }
  const { signIns } = renderApp(ACCOUNT_PATH, { api });
  try {
    await click(await screen.findByRole("tab", { name: entry.tab }));
    if (rolloverUsageMonth) {
      await waitFor(() => {
        const request = api.calls.find(
          ({ method }) => method === entry.method,
        )?.request;
        expect(request).toMatchObject({ ...USAGE_MONTH, pageToken: "" });
      });
      // Cross midnight after the page sends the request and before the fake serves it.
      rs.setSystemTime(UTC_ROLLOVER);
    }
  } finally {
    if (rolloverUsageMonth) {
      api.gates.delete(entry.method);
      releaseUsageResponse();
    }
  }
  await screen.findByText(entry.more, { selector: "button" });
  await waitFor(() => expect(rowCount(entry.list)).toBe(entry.firstPage));
  if (failure !== undefined) api.failContinuation.set(entry.method, failure);
  return { api, expectedRows, signIns };
}

for (const entry of lists) {
  describe(`${entry.name}: a failed next page`, () => {
    test("keeps the rows, shows the notice and retries the failed cursor once", async () => {
      const isUsageSummary = entry.method === "ListUsageSummary";
      const { api, expectedRows } = await openList(
        entry,
        new ConnectError("the page service is down", Code.Unavailable),
        { rolloverUsageMonth: isUsageSummary },
      );
      const firstPageRows = expectedRows.slice(0, entry.firstPage);
      expect(entry.readRowIdentities()).toEqual(firstPageRows);
      const usageTotal = () => screen.getByLabelText("Usage total").textContent;
      if (isUsageSummary) {
        expect(usageTotal()).toBe("$201.00");
        accountOf(api).usage.at(-1)!.amountUsdMicros = 3_000_000n;
      }

      await click(moreButton(entry.more)!);
      const notice = (await screen.findByText(UNAVAILABLE)).closest(
        '[role="alert"]',
      ) as HTMLElement;
      expect(entry.readRowIdentities()).toEqual(firstPageRows);
      if (isUsageSummary) expect(usageTotal()).toBe("$201.00");
      const requestsBeforeRetry = pageTokens(api, entry.method);
      expect(requestsBeforeRetry).toEqual(["", expect.stringMatching(/./)]);
      const failedToken = requestsBeforeRetry[1];
      // The button still reads on, and no request starts by itself.
      expect(moreButton(entry.more)!.disabled).toBe(false);

      await click(within(notice).getByRole("button", { name: TRY_AGAIN }));
      await waitFor(() =>
        expect(rowCount(entry.list)).toBe(expectedRows.length),
      );
      if (isUsageSummary) {
        expect(usageTotal()).toBe("$203.00");
        expect(usageRows().at(-1)?.cells[4]?.textContent).toBe("$3.00");
      }
      // The retry sent the failed cursor, not a refetch of the first page.
      expect(pageTokens(api, entry.method)).toEqual([
        ...requestsBeforeRetry,
        failedToken,
      ]);
      expect(screen.queryByText(UNAVAILABLE)).toBeNull();
      expect(moreButton(entry.more)).toBeNull();
      expect(entry.readRowIdentities()).toEqual(expectedRows);
    });

    test("an ended session keeps the rows and offers the sign-in action", async () => {
      const { api, signIns } = await openList(
        entry,
        new ConnectError("token expired", Code.Unauthenticated),
      );
      await click(moreButton(entry.more)!);
      const signIn = await screen.findByText(SIGN_IN, { selector: "button" });
      expect(rowCount(entry.list)).toBe(entry.firstPage);
      expect(screen.queryByText(TRY_AGAIN, { selector: "button" })).toBeNull();
      expect(pageTokens(api, entry.method)).toEqual([
        "",
        expect.stringMatching(/./),
      ]);
      await click(signIn);
      expect(signIns).toHaveLength(1);
      expect(rowCount(entry.list)).toBe(entry.firstPage);
    });

    test("a failed first request still replaces the section", async () => {
      const api = createFakeApi();
      api.userId = OWNER;
      entry.seed(accountOf(api));
      api.failNext.set(
        entry.method,
        new ConnectError("the page service is down", Code.Unavailable),
      );
      renderApp(ACCOUNT_PATH, { api });
      await click(await screen.findByRole("tab", { name: entry.tab }));
      await screen.findByText(UNAVAILABLE);
      expect(moreButton(entry.more)).toBeNull();
      if (entry.list !== undefined) {
        expect(screen.queryByRole("list", { name: entry.list })).toBeNull();
      }
    });
  });
}
