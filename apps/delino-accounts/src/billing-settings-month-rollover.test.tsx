// SPDX-License-Identifier: Apache-2.0
import { afterEach, expect, rs, test } from "@rstest/core";
import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  ACCOUNT_ID,
  ADMIN,
  createFakeApi,
  type FakeApi,
} from "./testing/fake-api.ts";
import { deferNextCall } from "./testing/form-submit-deferral.ts";
import { renderApp } from "./testing/render.tsx";

const ROLLOVER = Date.parse("2027-01-01T00:00:00Z");
const READ = "GetAutoTopUpSettings";

afterEach(() => {
  cleanup();
  rs.restoreAllMocks();
  rs.useRealTimers();
});

async function advance(ms = 100) {
  await act(async () => {
    await rs.advanceTimersByTimeAsync(ms);
    await new Promise((resolve) => rs.getRealTimers().setTimeout(resolve, 20));
  });
}

function setCapUsage(api: FakeApi, micros: bigint) {
  const account = api.accounts.get(ACCOUNT_ID)!;
  account.autoTopUp = { ...account.autoTopUp, monthToDateUsdMicros: micros };
}

function capUsage() {
  return screen.getByText("Monthly cap usage").nextSibling?.textContent;
}

function requestAccounts(api: FakeApi) {
  return api.calls
    .filter(({ method }) => method === READ)
    .map(({ request }) => request.billingAccountId);
}

function topUpAmount() {
  return screen.getByRole<HTMLInputElement>("textbox", {
    name: "Top-up amount (USD)",
  });
}

async function openSettings(msBeforeRollover: number, api = createFakeApi()) {
  rs.useFakeTimers({
    now: ROLLOVER - msBeforeRollover,
    toFake: ["Date", "setTimeout", "clearTimeout"],
  });
  api.userId = ADMIN;
  setCapUsage(api, 25_000_000n);
  const { queryClient } = renderApp(`/billing/${ACCOUNT_ID}`, { api });
  queryClient.setDefaultOptions({
    queries: { staleTime: 10_000, retry: false },
  });
  await advance();
  await advance();
  fireEvent.click(screen.getByRole("tab", { name: "Settings" }));
  await advance();
  await advance();
  return api;
}

test("refreshes the selected account on a foreground UTC rollover", async () => {
  const api = await openSettings(30_000);
  expect(capUsage()).toBe("$25.00");
  expect(requestAccounts(api)).toEqual([ACCOUNT_ID]);
  const amount = topUpAmount();
  fireEvent.input(amount, { target: { value: "75" } });
  setCapUsage(api, 0n);

  await advance(ROLLOVER - Date.now() - 1);
  expect(capUsage()).toBe("$25.00");
  await advance(1);
  await advance();
  expect(capUsage()).toBe("$0.00");
  expect(topUpAmount()).toBe(amount);
  expect(amount.value).toBe("75");

  await advance(60_000);
  expect(requestAccounts(api)).toEqual([ACCOUNT_ID, ACCOUNT_ID]);
  expect(
    api.calls.filter(({ method }) => method === "GetAlertSettings"),
  ).toHaveLength(1);
  expect(
    api.calls.filter(({ method }) => method === "UpdateAutoTopUpSettings"),
  ).toHaveLength(0);
});

test("refreshes still-fresh settings when visibility returns after a delayed timer", async () => {
  const api = await openSettings(5_000);
  expect(capUsage()).toBe("$25.00");
  const visibility = rs.spyOn(document, "visibilityState", "get");
  visibility.mockReturnValue("hidden");
  await act(async () => window.dispatchEvent(new Event("visibilitychange")));
  setCapUsage(api, 50_000_000n);

  rs.setSystemTime(ROLLOVER + 1);
  expect(requestAccounts(api)).toEqual([ACCOUNT_ID]);
  visibility.mockReturnValue("visible");
  await act(async () => window.dispatchEvent(new Event("visibilitychange")));
  await advance();

  expect(requestAccounts(api)).toEqual([ACCOUNT_ID, ACCOUNT_ID]);
  expect(capUsage()).toBe("$50.00");
});

test("keeps the draft when a rollover read fails and the user retries", async () => {
  const api = await openSettings(5_000);
  const amount = topUpAmount();
  fireEvent.input(amount, { target: { value: "75" } });
  setCapUsage(api, 0n);
  api.failNext.set(READ, new ConnectError("busy", Code.ResourceExhausted));

  await advance(ROLLOVER - Date.now());
  await advance();
  expect(capUsage()).toBe("$25.00");
  expect(topUpAmount()).toBe(amount);
  expect(amount.value).toBe("75");
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  await advance();

  expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
  expect(requestAccounts(api)).toEqual([ACCOUNT_ID, ACCOUNT_ID, ACCOUNT_ID]);
  expect(capUsage()).toBe("$0.00");
  expect(topUpAmount()).toBe(amount);
  expect(amount.value).toBe("75");
});

test("replaces an initial settings read that is still pending at rollover", async () => {
  const api = createFakeApi();
  const oldRead = deferNextCall(api, READ);
  await openSettings(5_000, api);
  expect(requestAccounts(api)).toEqual([ACCOUNT_ID]);
  expect(screen.queryByText("Monthly cap usage")).toBeNull();
  setCapUsage(api, 0n);

  await advance(ROLLOVER - Date.now());
  await advance();
  expect(requestAccounts(api)).toEqual([ACCOUNT_ID, ACCOUNT_ID]);
  expect(capUsage()).toBe("$0.00");

  setCapUsage(api, 25_000_000n);
  await act(async () => oldRead.release());
  await advance();
  expect(capUsage()).toBe("$0.00");
});
