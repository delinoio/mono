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
import { addBillingAccounts, createFakeApi } from "./testing/fake-api.ts";
import { listRows } from "./testing/list-rows.ts";
import { renderApp } from "./testing/render.tsx";

/**
 * The tab regains focus while a list that the app reads completely waits for
 * the user to retry a failed later page. The query of the test client is
 * stale at once (`staleTime` 0), so a focus refetches every query that the
 * list allows to refetch.
 */

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

/** Focuses the mounted tab, as the browser does when the user returns. */
async function focusTab() {
  await act(async () => {
    window.dispatchEvent(new Event("visibilitychange"));
  });
}

function pageTokens(api: ReturnType<typeof createFakeApi>) {
  return api.calls
    .filter((c) => c.method === "ListBillingAccounts")
    .map((c) => c.request.pageToken);
}

describe("focus of the tab while a complete list is read", () => {
  test("a failed later page waits for the retry after a focus", async () => {
    const api = createFakeApi();
    // The seeded account and 200 more, so that the second page has one row.
    addBillingAccounts(api, 200);
    api.failContinuation.set(
      "ListBillingAccounts",
      new ConnectError("unavailable", Code.Unavailable),
    );
    renderApp("/billing", { api });
    const notice = (await screen.findByText(/Some items did not load/)).closest(
      '[role="alert"]',
    ) as HTMLElement;
    const rows = () => listRows("Billing accounts");
    expect(rows()).toHaveLength(200);
    const requestsBeforeFocus = pageTokens(api);
    expect(requestsBeforeFocus).toEqual(["", expect.stringMatching(/./)]);
    const failedToken = requestsBeforeFocus[1];

    await focusTab();
    await focusTab();
    // The focus sends no request and keeps the rows and the notice.
    expect(pageTokens(api)).toEqual(requestsBeforeFocus);
    expect(rows()).toHaveLength(200);
    expect(screen.getByText(/Some items did not load/)).toBe(
      within(notice).getByText(/Some items did not load/),
    );

    // The retry requests the failed token first and the list then completes.
    await click(within(notice).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(rows()).toHaveLength(201));
    expect(pageTokens(api)).toEqual([...requestsBeforeFocus, failedToken]);
    expect(screen.queryByText(/Some items did not load/)).toBeNull();
  });

  test("a healthy stale list refreshes from the first page", async () => {
    const api = createFakeApi();
    addBillingAccounts(api, 200);
    renderApp("/billing", { api });
    await waitFor(() => expect(listRows("Billing accounts")).toHaveLength(201));
    expect(pageTokens(api)).toEqual(["", expect.stringMatching(/./)]);

    await focusTab();
    await waitFor(() => expect(pageTokens(api).length).toBeGreaterThan(2));
    // The refresh starts with the empty token and continues in order.
    expect(pageTokens(api).slice(2, 4)).toEqual([
      "",
      expect.stringMatching(/./),
    ]);
    expect(listRows("Billing accounts")).toHaveLength(201);
  });
});
