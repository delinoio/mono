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
  waitFor,
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
import { deferNextCall } from "./testing/form-submit-deferral.ts";
import { renderApp, settle } from "./testing/render.tsx";

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
  }) as HTMLButtonElement;
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

  test("keeps edits made during the save and its confirming read", async () => {
    const api = savedApi();
    const { amount, save } = await renderSettings(api);
    const toggle = screen.getByRole("switch", { name: "Automatic top-up" });
    const threshold = screen.getByRole("textbox", {
      name: "Threshold (USD)",
    }) as HTMLInputElement;
    const cap = screen.getByRole("textbox", {
      name: "Monthly cap (USD)",
    }) as HTMLInputElement;
    const mutation = deferNextCall(api, "UpdateAutoTopUpSettings");
    const confirmation = deferNextCall(api, "GetAutoTopUpSettings");
    const readsBeforeSave = api.calls.filter(
      (call) => call.method === "GetAutoTopUpSettings",
    ).length;

    await type(amount, "60");
    await click(save);
    expect(updates(api)).toEqual([
      expect.objectContaining({
        settings: expect.objectContaining({
          enabled: true,
          thresholdUsdMicros: 10_000_000n,
          amountUsdMicros: 60_000_000n,
          monthlyCapUsdMicros: 200_000_000n,
        }),
      }),
    ]);
    expect(save.disabled).toBe(true);
    await type(threshold, "15");
    await type(amount, "75");
    await type(cap, "250");
    await click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    await click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("true");

    await act(async () => mutation.release());
    await waitFor(() =>
      expect(
        api.calls.filter((call) => call.method === "GetAutoTopUpSettings"),
      ).toHaveLength(readsBeforeSave + 1),
    );
    expect([threshold.value, amount.value, cap.value]).toEqual([
      "15",
      "75",
      "250",
    ]);
    expect(save.disabled).toBe(true);
    expect(successToasts).not.toHaveBeenCalled();
    await type(amount, "80");

    await act(async () => confirmation.release());
    await waitFor(() => expect(save.disabled).toBe(false));
    expect(amount.isConnected).toBe(true);
    expect([threshold.value, amount.value, cap.value]).toEqual([
      "15",
      "80",
      "250",
    ]);
    expect(updates(api)).toHaveLength(1);
    await click(save);
    expect(updates(api)).toHaveLength(2);
    expect(updates(api)[1]).toEqual(
      expect.objectContaining({
        settings: expect.objectContaining({
          enabled: true,
          thresholdUsdMicros: 15_000_000n,
          amountUsdMicros: 80_000_000n,
          monthlyCapUsdMicros: 250_000_000n,
        }),
      }),
    );
    await waitFor(() => expect(amountValue()).toBe("80.00"));
  });

  test("an equivalent save preserves a newer switch edit and draft text", async () => {
    const api = savedApi();
    const { amount, save } = await renderSettings(api);
    const toggle = screen.getByRole("switch", { name: "Automatic top-up" });
    const mutation = deferNextCall(api, "UpdateAutoTopUpSettings");
    await type(amount, "50");
    await click(save);
    await click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("false");

    await act(async () => mutation.release());
    await waitFor(() => expect(save.disabled).toBe(false));
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(amountValue()).toBe("50");
    await click(save);
    expect(updates(api)).toHaveLength(2);
    expect(updates(api)[1]).toEqual(
      expect.objectContaining({
        settings: expect.objectContaining({ enabled: false }),
      }),
    );
  });

  test("a rejected save keeps the newer draft available for retry", async () => {
    const api = savedApi();
    const { amount, save } = await renderSettings(api);
    const mutation = deferNextCall(api, "UpdateAutoTopUpSettings");
    await type(amount, "60");
    await click(save);
    await type(amount, "75");
    await act(async () =>
      mutation.fail(new ConnectError("down", Code.Unavailable)),
    );
    expect(updates(api)).toEqual([
      expect.objectContaining({
        settings: expect.objectContaining({ amountUsdMicros: 60_000_000n }),
      }),
    ]);
    expect(
      await screen.findByText(/Delino Accounts is not available/),
    ).toBeTruthy();
    expect(amountValue()).toBe("75");
    expect(successToasts).not.toHaveBeenCalled();
    await click(save);
    expect(updates(api)).toHaveLength(2);
    expect(updates(api)[1]).toEqual(
      expect.objectContaining({
        settings: expect.objectContaining({ amountUsdMicros: 75_000_000n }),
      }),
    );
  });
});
