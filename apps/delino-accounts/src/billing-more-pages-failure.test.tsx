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
 * A failed request for a later page of a list that the app reads completely
 * (`useAllPages`, shown by `MorePages`). The loaded rows stay; an ended
 * session offers the sign-in action and any other failure offers a retry.
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

function pageTokens(api: ReturnType<typeof createFakeApi>) {
  return api.calls
    .filter((c) => c.method === "ListBillingAccounts")
    .map((c) => c.request.pageToken);
}

/** Renders the account list with a second page and arms its failure. */
async function renderFailingSecondPage(failure: ConnectError) {
  const api = createFakeApi();
  // The seeded account and 200 more, so that the second page has one row.
  addBillingAccounts(api, 200);
  api.failContinuation.set("ListBillingAccounts", failure);
  const { signIns } = renderApp("/billing", { api });
  const notice = (await screen.findByText(/Some items did not load/)).closest(
    '[role="alert"]',
  ) as HTMLElement;
  const rows = () => listRows("Billing accounts");
  return { api, signIns, notice, rows };
}

describe("a failed later page of a complete list", () => {
  test("an ended session keeps the rows and offers sign-in instead of a retry", async () => {
    const { api, signIns, notice, rows } = await renderFailingSecondPage(
      new ConnectError("token expired", Code.Unauthenticated),
    );
    expect(rows()).toHaveLength(200);
    expect(
      within(notice).queryByRole("button", { name: "Try again" }),
    ).toBeNull();
    await click(
      within(notice).getByRole("button", { name: "Sign in with Delino" }),
    );
    expect(signIns).toHaveLength(1);
    expect(rows()).toHaveLength(200);
    // The failure stops the loop and the sign-in action sends no request.
    expect(pageTokens(api)).toEqual(["", "p1"]);
  });

  test("another failure still retries at the failed page", async () => {
    const { api, notice, rows } = await renderFailingSecondPage(
      new ConnectError("unavailable", Code.Unavailable),
    );
    expect(rows()).toHaveLength(200);
    expect(
      within(notice).queryByRole("button", { name: "Sign in with Delino" }),
    ).toBeNull();
    await click(within(notice).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(rows()).toHaveLength(201));
    expect(pageTokens(api)).toEqual(["", "p1", "p1"]);
    expect(screen.queryByText(/Some items did not load/)).toBeNull();
  });
});
