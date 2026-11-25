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
import { renderApp, settle } from "./testing/render.tsx";

const noticeText = /Some items did not load/;
// The fake API uses offsets as tokens. A page of 200 rows ends at p200.
const initialTokens = ["", "p200"];

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

const lifecycleEvents = {
  focus: async () => {
    await act(async () => {
      window.dispatchEvent(new Event("visibilitychange"));
    });
  },
  reconnect: async () => {
    await act(async () => {
      window.dispatchEvent(new Event("offline"));
      window.dispatchEvent(new Event("online"));
    });
  },
  remount: async () => {
    await click(screen.getByRole("link", { name: "Delino Accounts" }));
    expect(screen.queryByRole("list", { name: "Billing accounts" })).toBeNull();
    // Client-side navigation remounts the list with the same QueryClient.
    await click(screen.getByRole("link", { name: "Manage billing" }));
  },
};

function requests(api: ReturnType<typeof createFakeApi>) {
  return api.calls
    .filter((c) => c.method === "ListBillingAccounts")
    .map((c) => c.request);
}

const pageTokens = (api: ReturnType<typeof createFakeApi>) =>
  requests(api).map((request) => request.pageToken);
const rows = () => listRows("Billing accounts");

function continuationNotice() {
  const notice = screen
    .getByText(noticeText)
    .closest<HTMLElement>('[role="alert"]');
  if (!notice) throw new Error("Missing continuation alert");
  return within(notice);
}

for (const [event, trigger] of Object.entries(lifecycleEvents)) {
  describe(`complete list lifecycle: ${event}`, () => {
    test("a failed later page waits for the retry", async () => {
      const api = createFakeApi();
      // The seeded account and 200 more, so that the second page has one row.
      addBillingAccounts(api, 200);
      api.failContinuation.set(
        "ListBillingAccounts",
        new ConnectError("unavailable", Code.Unavailable),
      );
      // The test client's default stale time is zero and retries are disabled.
      renderApp("/billing", { api });
      await screen.findByText(noticeText);
      expect(rows()).toHaveLength(200);
      expect(pageTokens(api)).toEqual(initialTokens);
      const failedRequest = requests(api)[1];
      expect(failedRequest.pageSize).toBe(200);

      await trigger();
      await settle();
      expect(pageTokens(api)).toEqual(initialTokens);
      expect(rows()).toHaveLength(200);
      await click(
        continuationNotice().getByRole("button", { name: "Try again" }),
      );
      await waitFor(() => expect(rows()).toHaveLength(201));
      expect(requests(api)[2]).toEqual(failedRequest);
      expect(pageTokens(api)).toEqual([...initialTokens, "p200"]);
      expect(screen.queryByText(noticeText)).toBeNull();
    });

    test("a healthy stale list refreshes from the first page", async () => {
      const api = createFakeApi();
      addBillingAccounts(api, 200);
      renderApp("/billing", { api });
      await waitFor(() => expect(rows()).toHaveLength(201));
      expect(pageTokens(api)).toEqual(initialTokens);

      await trigger();
      await waitFor(() =>
        expect(pageTokens(api)).toEqual([...initialTokens, ...initialTokens]),
      );
      expect(rows()).toHaveLength(201);
    });
  });
}

test("an expired session keeps its sign-in action across lifecycle events", async () => {
  const api = createFakeApi();
  addBillingAccounts(api, 200);
  api.failContinuation.set(
    "ListBillingAccounts",
    new ConnectError("token expired", Code.Unauthenticated),
  );
  const { signIns } = renderApp("/billing", { api });
  await screen.findByText(noticeText);

  for (const trigger of Object.values(lifecycleEvents)) {
    await trigger();
    await settle();
    expect(rows()).toHaveLength(200);
    expect(pageTokens(api)).toEqual(initialTokens);
    const notice = continuationNotice();
    expect(
      notice.getByRole("button", { name: "Sign in with Delino" }),
    ).toBeTruthy();
    expect(notice.queryByRole("button", { name: "Try again" })).toBeNull();
  }

  await click(screen.getByRole("button", { name: "Sign in with Delino" }));
  expect(signIns).toHaveLength(1);
});
