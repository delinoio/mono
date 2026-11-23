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
  within,
} from "@testing-library/react";
import { Code, ConnectError } from "@connectrpc/connect";
import { toast } from "delino-ui/toast";
import {
  ACCOUNT_ID,
  ADMIN,
  createFakeApi,
  type FakeApi,
} from "./testing/fake-api.ts";
import { renderApp, settle } from "./testing/render.tsx";

/**
 * The automatic top-up form shows the saved values after a successful save,
 * also when they are equivalent to the typed text ("50" and "50.00").
 */

async function click(element: Element) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle();
}

async function type(element: Element, value: string) {
  await act(async () => {
    fireEvent.input(element, { target: { value } });
  });
}

/** A fake API whose account has automatic top-up saved as 10.00/50.00/200.00. */
function savedApi(): FakeApi {
  const api = createFakeApi();
  api.userId = ADMIN;
  const account = api.accounts.get(ACCOUNT_ID)!;
  account.autoTopUp = {
    ...account.autoTopUp,
    enabled: true,
    thresholdUsdMicros: 10_000_000n,
    amountUsdMicros: 50_000_000n,
    monthlyCapUsdMicros: 200_000_000n,
  };
  return api;
}

async function renderSettings(api: FakeApi) {
  renderApp(`/billing/${ACCOUNT_ID}`, { api });
  await screen.findByRole("heading", { name: "Acme Robotics", level: 1 });
  await click(await screen.findByRole("tab", { name: "Settings" }));
  const amount = (await screen.findByRole("textbox", {
    name: "Top-up amount (USD)",
  })) as HTMLInputElement;
  const save = within(amount.closest("form")!).getByRole("button", {
    name: "Save",
  });
  return { amount, save };
}

function amountValue(): string {
  return (
    screen.getByRole("textbox", {
      name: "Top-up amount (USD)",
    }) as HTMLInputElement
  ).value;
}

function updates(api: FakeApi) {
  return api.calls
    .filter((c) => c.method === "UpdateAutoTopUpSettings")
    .map((c) => c.request);
}

let successToasts: ReturnType<typeof rs.spyOn>;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  successToasts = rs.spyOn(toast, "success");
});

afterEach(() => {
  cleanup();
  successToasts.mockRestore();
});

describe("automatic top-up save", () => {
  test("a successful save replaces an equivalent amount text with the saved value", async () => {
    const api = savedApi();
    const { amount, save } = await renderSettings(api);
    expect(amountValue()).toBe("50.00");
    await type(amount, "50");
    expect(amountValue()).toBe("50");
    await click(save);
    await settle(60);
    expect(updates(api)).toEqual([
      expect.objectContaining({
        settings: expect.objectContaining({ amountUsdMicros: 50_000_000n }),
      }),
    ]);
    expect(successToasts.mock.calls.map((call) => String(call[0]))).toEqual([
      "Automatic top-up is saved.",
    ]);
    expect(amountValue()).toBe("50.00");
  });

  test("a rejected save keeps the draft and shows the error", async () => {
    const api = savedApi();
    const { amount, save } = await renderSettings(api);
    await type(amount, "60");
    api.failNext.set(
      "UpdateAutoTopUpSettings",
      new ConnectError("down", Code.Unavailable),
    );
    await click(save);
    await settle(60);
    expect(updates(api)).toHaveLength(1);
    expect(
      await screen.findByText(/Delino Accounts is not available/),
    ).toBeTruthy();
    expect(amountValue()).toBe("60");
    expect(successToasts).not.toHaveBeenCalled();
  });
});
