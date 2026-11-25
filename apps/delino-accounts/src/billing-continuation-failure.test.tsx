// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
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

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
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
 * usage table rows are the `tr` elements of the table.
 */
function rowCount(list: string | undefined) {
  if (list === undefined) {
    const table = document.querySelector("table");
    if (table === null) throw new Error("no table");
    return table.querySelectorAll("tr").length - 1;
  }
  return listRows(list).length;
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
    list: "Invoices" as string | undefined,
    more: "Show older invoices",
    total: 201,
    firstPage: 200,
    seed: (account: ReturnType<typeof accountOf>) =>
      addInvoices(account, 201 - account.invoices.length),
  },
  {
    name: "ledger",
    method: "ListLedgerEntries",
    tab: "Usage",
    list: "Ledger entries" as string | undefined,
    more: "Show older entries",
    total: 61,
    firstPage: 50,
    seed: (account: ReturnType<typeof accountOf>) =>
      addLedgerEntries(account, 61 - account.ledger.length),
  },
  {
    name: "usage summary",
    method: "ListUsageSummary",
    tab: "Usage",
    list: undefined as string | undefined,
    more: "Show more usage",
    total: 201,
    firstPage: 200,
    seed: (account: ReturnType<typeof accountOf>) =>
      addUsageRows(account, 201 - account.usage.length),
  },
];

/** Opens the list with its first page loaded and `failure` armed. */
async function openList(
  entry: (typeof lists)[number],
  failure: ConnectError | undefined,
) {
  const api = createFakeApi();
  api.userId = OWNER;
  entry.seed(accountOf(api));
  const { signIns } = renderApp(ACCOUNT_PATH, { api });
  await click(await screen.findByRole("tab", { name: entry.tab }));
  await screen.findByText(entry.more, { selector: "button" });
  await waitFor(() => expect(rowCount(entry.list)).toBe(entry.firstPage));
  if (failure !== undefined) api.failContinuation.set(entry.method, failure);
  return { api, signIns };
}

for (const entry of lists) {
  describe(`${entry.name}: a failed next page`, () => {
    test("keeps the rows, shows the notice and retries the failed cursor once", async () => {
      const { api } = await openList(
        entry,
        new ConnectError("the page service is down", Code.Unavailable),
      );
      await click(moreButton(entry.more)!);
      const notice = (await screen.findByText(UNAVAILABLE)).closest(
        '[role="alert"]',
      ) as HTMLElement;
      expect(rowCount(entry.list)).toBe(entry.firstPage);
      const requestsBeforeRetry = pageTokens(api, entry.method);
      expect(requestsBeforeRetry).toEqual(["", expect.stringMatching(/./)]);
      const failedToken = requestsBeforeRetry[1];
      // The button still reads on, and no request starts by itself.
      expect(moreButton(entry.more)!.disabled).toBe(false);

      await click(within(notice).getByRole("button", { name: TRY_AGAIN }));
      await waitFor(() => expect(rowCount(entry.list)).toBe(entry.total));
      // The retry sent the failed cursor, not a refetch of the first page.
      expect(pageTokens(api, entry.method)).toEqual([
        ...requestsBeforeRetry,
        failedToken,
      ]);
      expect(screen.queryByText(UNAVAILABLE)).toBeNull();
      expect(moreButton(entry.more)).toBeNull();
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
