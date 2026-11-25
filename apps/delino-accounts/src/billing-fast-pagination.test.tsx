// SPDX-License-Identifier: Apache-2.0
import { createRouterTransport } from "@connectrpc/connect";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import { expect, test } from "@rstest/core";
import { defaultScheduler, notifyManager } from "@tanstack/react-query";
import { act, cleanup, screen, within } from "@testing-library/react";
import { createFakeApi } from "./testing/fake-api.ts";
import { renderApp } from "./testing/render.tsx";

test("follows every token when React renders only the settled pages", async () => {
  const tokens = ["", "p1", "p2", "p3"];
  const names = ["Acme", "Bravo", "Cedar", "Delta"];
  const requests: string[] = [];
  const api = createFakeApi();
  api.transport = createRouterTransport(({ service }) => {
    service(BillingAccountService, {
      listBillingAccounts({ pageToken }) {
        requests.push(pageToken);
        const page = tokens.indexOf(pageToken);
        return {
          billingAccounts: [
            { billingAccountId: String(page), name: names[page] },
          ],
          nextPageToken: tokens[page + 1] ?? "",
        };
      },
    });
  });

  // Let each request finish before notifying React. A single render sees the
  // successful data and idle state together, with no fetching render.
  const notifications: Array<() => void> = [];
  notifyManager.setScheduler((notify) => notifications.push(notify));
  try {
    await act(async () => {
      renderApp("/billing", { api });
    });
    for (let page = 0; page < names.length; page++) {
      await act(async () => {
        for (const notify of notifications.splice(0)) notify();
      });
      const list = screen.getByRole("list", { name: "Billing accounts" });
      expect(within(list).getAllByRole("listitem")).toHaveLength(page + 1);
      expect(within(list).getByText(names[page])).toBeTruthy();
    }
    expect(requests).toEqual(tokens);
    expect(screen.queryByText("Loading more")).toBeNull();
  } finally {
    cleanup();
    notifyManager.setScheduler(defaultScheduler);
  }
});
