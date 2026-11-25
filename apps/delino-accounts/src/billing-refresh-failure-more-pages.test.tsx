// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import { addBillingAccounts, createFakeApi } from "./testing/fake-api.ts";
import { listRows } from "./testing/list-rows.ts";
import { renderApp } from "./testing/render.tsx";

/**
 * A refresh of a partially loaded complete list fails while the cache still
 * holds the continuation token of its last loaded page. The list must pause
 * like it does for a failed page: keep the rows and the refresh retry, hide
 * the "Loading more" status and start no continuation request until the
 * user retries.
 */

const LOADING_MORE = "Loading more";
const REFRESH_NOTICE = "Too many requests. Wait a moment and try again.";

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

const rows = () => listRows("Billing accounts");

/**
 * A gate that lets the first request through and holds every later one until
 * `release`, so the test sees the state while the second request is active
 * without a timer.
 */
function holdAfterFirstRequest() {
  let seen = 0;
  const held: Array<() => void> = [];
  return {
    gate: {
      then(resolve: () => void) {
        seen += 1;
        if (seen === 1) resolve();
        else held.push(resolve);
      },
    } as unknown as Promise<unknown>,
    release() {
      for (const resolve of held.splice(0)) resolve();
    },
  };
}

test("a failed refresh of a partially loaded list pauses the continuation until the retry", async () => {
  const api = createFakeApi();
  // The seeded account and 200 more: a second page with one row.
  addBillingAccounts(api, 200);
  // Fail the second page, so that the cache holds page one with its token.
  api.failContinuation.set(
    "ListBillingAccounts",
    new ConnectError("unavailable", Code.Unavailable),
  );
  const { queryClient } = renderApp("/billing", { api });
  await screen.findByText(/Some items did not load/);
  expect(rows()).toHaveLength(200);
  const initialTokens = pageTokens(api);
  expect(initialTokens).toEqual(["", expect.stringMatching(/./)]);

  // Explicitly refresh the active list from page one and fail that request.
  api.failNext.set(
    "ListBillingAccounts",
    new ConnectError("busy", Code.ResourceExhausted),
  );
  await act(async () => {
    await queryClient.refetchQueries({
      type: "active",
      queryKey: createConnectQueryKey({
        schema: BillingAccountService.method.listBillingAccounts,
        cardinality: undefined,
      }),
    });
  });
  const notice = (await screen.findByText(REFRESH_NOTICE)).closest(
    '[role="alert"]',
  ) as HTMLElement;
  // The rows and the refresh retry stay, and nothing claims more is loading.
  expect(rows()).toHaveLength(200);
  expect(
    within(notice).getByRole("button", { name: "Try again" }),
  ).toBeTruthy();
  expect(screen.queryByText(LOADING_MORE)).toBeNull();
  // The refresh made one request, and no continuation request follows.
  expect(pageTokens(api)).toEqual([...initialTokens, ""]);

  // The retry reads page one, then resumes with the second page.
  const held = holdAfterFirstRequest();
  api.gates.set("ListBillingAccounts", held.gate);
  await click(within(notice).getByRole("button", { name: "Try again" }));
  await waitFor(() =>
    expect(pageTokens(api)).toEqual([
      ...initialTokens,
      "",
      "",
      expect.stringMatching(/./),
    ]),
  );
  const retryTokens = pageTokens(api);
  // The second page request is active: the status shows again.
  expect((await screen.findByText(LOADING_MORE)).getAttribute("role")).toBe(
    "status",
  );
  api.gates.delete("ListBillingAccounts");
  await act(async () => {
    held.release();
  });
  await waitFor(() => expect(rows()).toHaveLength(201));
  expect(screen.queryByText(LOADING_MORE)).toBeNull();
  expect(screen.queryByText(REFRESH_NOTICE)).toBeNull();
  expect(pageTokens(api)).toEqual(retryTokens);
});
