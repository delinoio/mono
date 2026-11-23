// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { SubscriptionStatus } from "@delino/accounts-protos/v1/credit_pb";
import {
  ACCOUNT_ID,
  addBillingAccounts,
  createFakeApi,
  numbered,
} from "./testing/fake-api.ts";
import { deferNextCall } from "./testing/form-submit-deferral.ts";
import { fakeCheckout, renderApp, settle } from "./testing/render.tsx";

// A subscription start is an asynchronous request. These tests hold its
// response, move to another billing account, and release it, to check that a
// response of the account the user left does not open a checkout.

function startableApi() {
  const api = createFakeApi();
  api.accounts.get(ACCOUNT_ID)!.subscription = {
    status: SubscriptionStatus.NONE,
    cancelAtPeriodEnd: false,
  };
  addBillingAccounts(api, 1);
  const secondId = [...api.accounts.keys()].find((id) => id !== ACCOUNT_ID)!;
  return { api, secondId };
}

async function startSubscription() {
  const button = await screen.findByRole("button", {
    name: "Start subscription",
  });
  await act(async () => {
    fireEvent.click(button);
  });
  await settle();
}

async function goTo(accountId: string) {
  await act(async () => {
    window.history.pushState(null, "", `/billing/${accountId}`);
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

describe("subscription checkout", () => {
  for (const strict of [false, true]) {
    test(`opens the checkout once for the current account${strict ? " in StrictMode" : ""}`, async () => {
      const { api } = startableApi();
      const held = deferNextCall(api, "StartSubscription");
      const checkout = fakeCheckout();
      renderApp(`/billing/${ACCOUNT_ID}`, { api, checkout, strict });
      await startSubscription();
      expect(checkout.opened).toEqual([]);
      held.release();
      await settle(60);
      expect(checkout.opened).toEqual(["txn_checkout1"]);
      expect(checkout.signals).toHaveLength(1);
      expect(checkout.signals[0]?.aborted).toBe(false);
    });
  }

  test("does not open a checkout when the response arrives after the user left the account", async () => {
    const { api, secondId } = startableApi();
    const held = deferNextCall(api, "StartSubscription");
    const checkout = fakeCheckout();
    renderApp(`/billing/${ACCOUNT_ID}`, { api, checkout });
    await startSubscription();
    await goTo(secondId);
    await screen.findByRole("heading", {
      name: `Account ${numbered(1)}`,
      level: 1,
    });
    held.release();
    await settle(60);
    expect(checkout.opened).toEqual([]);
    expect(checkout.signals).toEqual([]);
    // The request itself completed; only the checkout was dropped.
    expect(
      api.calls.filter((c) => c.method === "StartSubscription"),
    ).toHaveLength(1);
  });

  test("a later checkout for the current account still opens after a stale one was dropped", async () => {
    const { api, secondId } = startableApi();
    const held = deferNextCall(api, "StartSubscription");
    const checkout = fakeCheckout();
    renderApp(`/billing/${ACCOUNT_ID}`, { api, checkout });
    await startSubscription();
    await goTo(secondId);
    await screen.findByRole("heading", {
      name: `Account ${numbered(1)}`,
      level: 1,
    });
    held.release();
    await settle(60);
    expect(checkout.opened).toEqual([]);
    // Back on the first account, the started subscription is PENDING, so
    // the page offers to continue its checkout, which opens now.
    await goTo(ACCOUNT_ID);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Continue checkout" }),
      );
    });
    await settle(60);
    expect(checkout.opened).toEqual(["txn_checkout1"]);
  });

  test("aborts the signal of an open checkout call when the user leaves the account", async () => {
    const { api, secondId } = startableApi();
    const checkout = fakeCheckout();
    renderApp(`/billing/${ACCOUNT_ID}`, { api, checkout });
    await startSubscription();
    await settle(60);
    expect(checkout.signals).toHaveLength(1);
    expect(checkout.signals[0]?.aborted).toBe(false);
    await goTo(secondId);
    await settle(60);
    expect(checkout.signals[0]?.aborted).toBe(true);
  });
});
