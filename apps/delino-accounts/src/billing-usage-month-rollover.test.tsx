// SPDX-License-Identifier: Apache-2.0
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { ACCOUNT_ID, createFakeApi, VIEWER } from "./testing/fake-api.ts";
import { renderApp } from "./testing/render.tsx";

/**
 * The upper limit of the usage month picker follows the clock while the page
 * is mounted. Every test runs on a fake clock, because a real month rollover
 * cannot be waited for; no test relies on the length of a real timer.
 */

/** Enough fake time for the loading skeleton delay and the first reads. */
const SETTLE_MS = 100;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  rs.useRealTimers();
});

/** Runs the fake clock forward inside `act`, flushing pending requests. */
async function advance(ms: number) {
  await act(async () => {
    await rs.advanceTimersByTimeAsync(ms);
    await new Promise((resolve) => rs.getRealTimers().setTimeout(resolve, 20));
  });
}

/** Renders the Usage tab at the fake time `now` and lets the reads finish. */
async function renderUsage(now: string) {
  rs.useFakeTimers({
    now: new Date(now),
    toFake: [
      "setTimeout",
      "clearTimeout",
      "setInterval",
      "clearInterval",
      "Date",
    ],
  });
  const api = createFakeApi();
  api.userId = VIEWER;
  renderApp(`/billing/${ACCOUNT_ID}`, { api });
  await advance(SETTLE_MS);
  await advance(SETTLE_MS);
  await act(async () => {
    fireEvent.click(screen.getByRole("tab", { name: "Usage" }));
  });
  await advance(SETTLE_MS);
  return api;
}

const nextButton = () =>
  screen.getByRole("button", { name: "Next month" }) as HTMLButtonElement;

const monthLabel = () => document.querySelector(".month-label")?.textContent;

function summaryMonths(api: ReturnType<typeof createFakeApi>) {
  return api.calls
    .filter((c) => c.method === "ListUsageSummary")
    .map((c) => [c.request.year, c.request.month]);
}

/** Reports a return to the visible tab, like the browser does. */
async function returnToTab() {
  await act(async () => {
    window.dispatchEvent(new Event("visibilitychange"));
  });
  await advance(SETTLE_MS);
}

describe("usage month limit", () => {
  test("Next becomes available when the UTC month changes while the page is open", async () => {
    const api = await renderUsage("2026-12-31T23:59:59Z");
    expect(monthLabel()).toBe("December 2026 (UTC)");
    expect(nextButton().disabled).toBe(true);

    await advance(1000);
    // The selected month stays and its data is not read again.
    expect(monthLabel()).toBe("December 2026 (UTC)");
    expect(nextButton().disabled).toBe(false);
    expect(new Set(summaryMonths(api).map(String))).toEqual(
      new Set(["2026,12"]),
    );

    // The new current month is requested and is the limit again.
    await act(async () => {
      fireEvent.click(nextButton());
    });
    await advance(SETTLE_MS);
    expect(monthLabel()).toBe("January 2027 (UTC)");
    expect(summaryMonths(api).at(-1)).toEqual([2027, 1]);
    expect(nextButton().disabled).toBe(true);
  });

  test("a forward clock correction refreshes the limit within a minute while visible", async () => {
    const api = await renderUsage("2026-12-15T12:00:00Z");
    expect(monthLabel()).toBe("December 2026 (UTC)");
    expect(nextButton().disabled).toBe(true);
    const initialRequests = summaryMonths(api);
    expect(initialRequests).toEqual([[2026, 12]]);

    rs.setSystemTime(new Date("2027-01-10T08:00:00Z"));
    expect(nextButton().disabled).toBe(true);
    await advance(60_000);
    expect(nextButton().disabled).toBe(false);
    expect(monthLabel()).toBe("December 2026 (UTC)");
    expect(summaryMonths(api)).toEqual(initialRequests);

    await act(async () => {
      fireEvent.click(nextButton());
    });
    await advance(SETTLE_MS);
    expect(monthLabel()).toBe("January 2027 (UTC)");
    expect(summaryMonths(api)).toEqual([
      [2026, 12],
      [2027, 1],
    ]);
    expect(nextButton().disabled).toBe(true);
  });

  test("a scheduled recheck within the same month keeps the limit and query", async () => {
    const api = await renderUsage("2026-12-15T12:00:00Z");
    rs.setSystemTime(new Date("2026-12-20T12:00:00Z"));
    await advance(60_000);
    expect(nextButton().disabled).toBe(true);
    expect(monthLabel()).toBe("December 2026 (UTC)");
    expect(summaryMonths(api)).toEqual([[2026, 12]]);
  });

  test("a return to the tab updates the limit although the timers were delayed", async () => {
    const api = await renderUsage("2026-12-15T12:00:00Z");
    expect(nextButton().disabled).toBe(true);

    // The clock moves into January 2027 without any timer firing, like a
    // hidden tab whose timers the browser throttled or suspended.
    rs.setSystemTime(new Date("2027-01-10T08:00:00Z"));
    expect(nextButton().disabled).toBe(true);
    await returnToTab();
    expect(monthLabel()).toBe("December 2026 (UTC)");
    expect(nextButton().disabled).toBe(false);

    await act(async () => {
      fireEvent.click(nextButton());
    });
    await advance(SETTLE_MS);
    expect(monthLabel()).toBe("January 2027 (UTC)");
    expect(summaryMonths(api).at(-1)).toEqual([2027, 1]);
    // January is the current month; February is not selectable.
    expect(nextButton().disabled).toBe(true);
  });

  test("a hidden tab does not change the limit until it is visible", async () => {
    await renderUsage("2026-12-15T12:00:00Z");
    rs.setSystemTime(new Date("2027-01-10T08:00:00Z"));
    const visibility = Object.getOwnPropertyDescriptor(
      document,
      "visibilityState",
    );
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => "hidden",
    });
    try {
      await returnToTab();
      await advance(60_000);
      expect(nextButton().disabled).toBe(true);
    } finally {
      if (visibility === undefined) {
        Reflect.deleteProperty(document, "visibilityState");
      } else {
        Object.defineProperty(document, "visibilityState", visibility);
      }
    }
    await returnToTab();
    expect(nextButton().disabled).toBe(false);
  });
});
