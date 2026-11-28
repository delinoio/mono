// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import {
  BillingMeter,
  BillingService,
  type GetBillingUsageResponse,
  type UnlinkBillingAccountRequest,
} from "@delino/ownsand-protos/v1/billing_pb";
import { QueryClient } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import {
  MOCK_IDS,
  MockBillingState,
  createMockTransport,
} from "./mock/mock-api.ts";
import {
  DEFAULT_WAIT_MS,
  TEST_SETUP_MS,
  holdingTransport,
  renderConsole,
} from "./test-support.tsx";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };
const UNLINK_ERROR_WAIT_MS = 3_000;

const ADMIN = "user-admin-jun";
const SETTINGS = `/orgs/${MOCK_IDS.acme}/settings`;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  toast.dismiss();
});

/**
 * A transport whose GetBillingUsage fails with `unavailable` while
 * `state.failUsage` is set. It records every request per method name.
 */
function usageFailingTransport(mock: Transport = createMockTransport()) {
  const state = { failUsage: true, calls: {} as Record<string, number> };
  const transport: Transport = {
    ...mock,
    async unary(method, ...rest) {
      state.calls[method.name] = (state.calls[method.name] ?? 0) + 1;
      if (method.name === "GetBillingUsage" && state.failUsage) {
        throw new ConnectError("usage is unavailable", Code.Unavailable);
      }
      return mock.unary(method, ...rest);
    },
  };
  return { transport, state };
}

const billing = () => screen.findByRole("region", { name: "Billing" });

async function click(element: HTMLElement) {
  await act(async () => {
    fireEvent.click(element);
  });
}

describe("the unlink action without usage", () => {
  test(
    "the owner can unlink while the usage is loading",
    async () => {
      const { transport, calls } = holdingTransport(["GetBillingUsage"]);
      renderConsole(SETTINGS, { transport });
      const section = await billing();
      await within(section).findByRole("status", {
        name: "Loading the billing usage",
      });
      await waitFor(() => expect(calls.GetBillingUsage).toHaveLength(1), WAIT);
      const unlink = within(section).getByRole("button", {
        name: "Unlink billing account",
      });
      await click(unlink);
      const dialog = await screen.findByRole("alertdialog");
      expect(
        within(dialog).getByRole("heading", {
          name: "Unlink the billing account?",
        }),
      ).toBeTruthy();
      await act(async () => {
        for (const call of calls.GetBillingUsage) call.settle();
      });
    },
    WAIT.timeout + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "the owner can open the confirmation after a usage error, and the retry stays",
    async () => {
      const { transport, state } = usageFailingTransport();
      renderConsole(SETTINGS, { transport });
      const section = await billing();
      const retry = await within(section).findByRole("button", {
        name: "Try again",
      });
      const unlink = within(section).getByRole("button", {
        name: "Unlink billing account",
      });
      await click(unlink);
      const dialog = await screen.findByRole("alertdialog");
      expect(
        within(dialog).getByRole("heading", {
          name: "Unlink the billing account?",
        }),
      ).toBeTruthy();
      await click(within(dialog).getByRole("button", { name: "Cancel" }));
      await waitFor(
        () => expect(screen.queryByRole("alertdialog")).toBeNull(),
        WAIT,
      );
      expect(state.calls.UnlinkBillingAccount).toBeUndefined();

      // The retry loads the usage, and the unlink action stays.
      state.failUsage = false;
      await click(retry);
      await within(section).findByText(
        "Acme Robotics billing",
        undefined,
        WAIT,
      );
      expect(
        within(section).getByRole("button", { name: "Unlink billing account" }),
      ).toBeTruthy();
      expect(
        within(section).queryByRole("button", { name: "Try again" }),
      ).toBeNull();
    },
    2 * WAIT.timeout + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "confirming after a usage error unlinks once and shows the unlinked view",
    async () => {
      const { transport, state } = usageFailingTransport();
      renderConsole(SETTINGS, { transport });
      const section = await billing();
      await click(
        await within(section).findByRole("button", {
          name: "Unlink billing account",
        }),
      );
      const dialog = await screen.findByRole("alertdialog");
      await click(
        within(dialog).getByRole("button", { name: "Unlink billing account" }),
      );
      await within(section).findByRole(
        "button",
        { name: "Link a billing account" },
        WAIT,
      );
      expect(state.calls.UnlinkBillingAccount).toBe(1);
      expect(screen.queryByRole("alertdialog")).toBeNull();
      expect(
        within(section).queryByRole("button", {
          name: "Unlink billing account",
        }),
      ).toBeNull();
      expect(
        within(section).queryByRole("button", { name: "Try again" }),
      ).toBeNull();
    },
    WAIT.timeout + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "relinking hides stale usage and preserves the captured unlink target",
    async () => {
      const mock = createMockTransport();
      const client = createClient(BillingService, mock);
      const organizationId = MOCK_IDS.acme;
      const accountA = MOCK_IDS.acmeBillingAccount;
      const accountB = MOCK_IDS.labBillingAccount;
      const queryClient = new QueryClient({
        defaultOptions: { queries: { retry: false } },
      });
      const requestedIds: (string | undefined)[] = [];
      let statusCalls = 0;
      let holdUsage = false;
      let failUsage = false;
      const fixture: Transport = {
        ...mock,
        async unary(method, ...rest) {
          if (method.name === "GetBillingStatus") statusCalls++;
          if (method.name === "UnlinkBillingAccount") {
            requestedIds.push(
              (rest[3] as UnlinkBillingAccountRequest).expectedBillingAccountId,
            );
          }
          if (method.name === "GetBillingUsage" && failUsage) {
            throw new ConnectError("usage is unavailable", Code.Unavailable);
          }
          const response = await mock.unary(method, ...rest);
          if (method.name === "GetBillingUsage") {
            const usage =
              response.message as unknown as GetBillingUsageResponse;
            if (usage.billingAccountId === accountB) {
              usage.meterUsages = [
                {
                  ...usage.meterUsages[0]!,
                  meter: BillingMeter.CLASS_A_RPC,
                  quantity: 9_999n,
                  amountUsdMicros: 2_500_000n,
                },
              ];
            }
          }
          return response;
        },
      };
      const { transport, calls } = holdingTransport(
        ["GetBillingUsage"],
        () => holdUsage,
        fixture,
      );
      renderConsole(SETTINGS, { transport, queryClient });
      const section = await billing();
      const content = within(section);
      const table = { name: "Usage this month" };
      await content.findByText("Acme Robotics billing", undefined, WAIT);
      expect(content.getByText("$42.52")).toBeTruthy();
      expect(content.getByText("$1.17")).toBeTruthy();
      expect(content.getByRole("table", table)).toBeTruthy();

      // The dialog's captured name is outside the usage section.
      const expectPreviousUsageHidden = () => {
        expect(content.queryByText("Acme Robotics billing")).toBeNull();
        expect(content.queryByText("$42.52")).toBeNull();
        expect(content.queryByText("$1.17")).toBeNull();
        expect(content.queryByRole("table", table)).toBeNull();
      };
      const unlink = () =>
        content.getByRole("button", { name: "Unlink billing account" });
      const retry = { name: "Try again" };
      const confirm = (dialog: HTMLElement) =>
        click(
          within(dialog).getByRole("button", {
            name: "Unlink billing account",
          }),
        );

      await click(unlink());
      const opening = await screen.findByRole("alertdialog", {
        name: "Unlink Acme Robotics billing?",
      });
      await client.unlinkBillingAccount({ organizationId });
      await client.linkBillingAccount({
        organizationId,
        billingAccountId: accountB,
      });
      await act(async () => {
        await queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({
            schema: BillingService.method.getBillingStatus,
            cardinality: undefined,
          }),
        });
      });
      await content.findByText("Something went wrong. Try again.");
      expectPreviousUsageHidden();
      expect(
        within(opening).getByRole("heading", {
          name: "Unlink Acme Robotics billing?",
        }),
      ).toBeTruthy();

      holdUsage = true;
      const usageRefresh = queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({
          schema: BillingService.method.getBillingUsage,
          cardinality: undefined,
        }),
      });
      await content.findByRole("status", {
        name: "Loading the billing usage",
      });
      expectPreviousUsageHidden();
      holdUsage = false;
      failUsage = true;
      await act(async () => {
        calls.GetBillingUsage![0]!.settle(
          new ConnectError("usage is unavailable", Code.Unavailable),
        );
        await usageRefresh;
      });
      expectPreviousUsageHidden();

      const priorStatusCalls = statusCalls;
      await confirm(opening);
      await screen.findByText(
        "The linked billing account changed. Review the current account and confirm again.",
      );
      await waitFor(
        () => expect(screen.queryByRole("alertdialog")).toBeNull(),
        WAIT,
      );
      expect(statusCalls).toBeGreaterThan(priorStatusCalls);
      expect(requestedIds).toEqual([accountA]);
      expect(
        (await client.getBillingStatus({ organizationId })).billingAccountId,
      ).toBe(accountB);
      expectPreviousUsageHidden();
      expect(content.getAllByRole("button", retry)).toHaveLength(1);

      await click(unlink());
      const newOpening = await screen.findByRole("alertdialog", {
        name: "Unlink the billing account?",
      });
      await click(within(newOpening).getByRole("button", { name: "Cancel" }));
      failUsage = false;
      await click(await content.findByRole("button", retry));
      await content.findByText("Research lab credits", undefined, WAIT);
      expect(content.getByText("-$1.23 (debt)")).toBeTruthy();
      expect(content.getByText("$2.50", { selector: "dd" })).toBeTruthy();
      expect(
        within(content.getByRole("table", table)).getByText("9,999 calls"),
      ).toBeTruthy();
      expect(content.queryByRole("button", retry)).toBeNull();

      await click(unlink());
      await confirm(
        await screen.findByRole("alertdialog", {
          name: "Unlink Research lab credits?",
        }),
      );
      await content.findByRole(
        "button",
        { name: "Link a billing account" },
        WAIT,
      );
      expect(requestedIds).toEqual([accountA, accountB]);
    },
    4 * WAIT.timeout + 8 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test("a transient refresh failure keeps usage for the same account", async () => {
    const { transport, state } = usageFailingTransport();
    state.failUsage = false;
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    renderConsole(SETTINGS, { transport, queryClient });
    const content = within(await billing());
    await content.findByText("Acme Robotics billing");

    state.failUsage = true;
    await act(() =>
      queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({
          schema: BillingService.method.getBillingUsage,
          cardinality: undefined,
        }),
      }),
    );
    await content.findByRole("button", { name: "Try again" });
    expect(content.getByText("Acme Robotics billing")).toBeTruthy();
    expect(
      content.getByRole("table", { name: "Usage this month" }),
    ).toBeTruthy();
  });

  test("an admin sees usage when status omits the account ID", async () => {
    const transport = createMockTransport({ userId: ADMIN });
    const client = createClient(BillingService, transport);
    const status = await client.getBillingStatus({
      organizationId: MOCK_IDS.acme,
    });
    expect(status.billingAccountId).toBeUndefined();
    renderConsole(SETTINGS, { transport });
    const content = within(await billing());
    await content.findByText("Acme Robotics billing");
    expect(
      content.getByRole("table", { name: "Usage this month" }),
    ).toBeTruthy();
  });

  test("an unlinked organization skips usage", async () => {
    const { transport, state } = usageFailingTransport(
      createMockTransport({
        billing: { [MOCK_IDS.acme]: MockBillingState.BlockedUnlinked },
      }),
    );
    renderConsole(SETTINGS, { transport });
    const section = await billing();
    await within(section).findByRole("button", {
      name: "Link a billing account",
    });
    expect(state.calls.GetBillingUsage).toBeUndefined();
  });

  test("the owner cannot confirm with a status that has no linked account ID", async () => {
    const mock = createMockTransport();
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        const response = await mock.unary(method, ...rest);
        return method.name === "GetBillingStatus"
          ? {
              ...response,
              message: { ...response.message, billingAccountId: undefined },
            }
          : response;
      },
    };
    renderConsole(SETTINGS, { transport });
    const section = await billing();
    const unlink = await within(section).findByRole("button", {
      name: "Unlink billing account",
    });
    expect((unlink as HTMLButtonElement).disabled).toBe(true);
    await click(unlink);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  test(
    "a failed unlink keeps the confirmation open with the error",
    async () => {
      const base = usageFailingTransport();
      const transport: Transport = {
        ...base.transport,
        async unary(method, ...rest) {
          if (method.name === "UnlinkBillingAccount") {
            throw new ConnectError("the unlink failed", Code.Unavailable);
          }
          return base.transport.unary(method, ...rest);
        },
      };
      renderConsole(SETTINGS, { transport });
      const section = await billing();
      await click(
        await within(section).findByRole("button", {
          name: "Unlink billing account",
        }),
      );
      const dialog = await screen.findByRole("alertdialog");
      await click(
        within(dialog).getByRole("button", { name: "Unlink billing account" }),
      );
      await waitFor(
        () =>
          expect(
            screen.getByRole("alertdialog").querySelector(".notice-error"),
          ).not.toBeNull(),
        { timeout: UNLINK_ERROR_WAIT_MS },
      );
      expect(
        within(section).queryByRole("button", {
          name: "Link a billing account",
        }),
      ).toBeNull();
    },
    UNLINK_ERROR_WAIT_MS + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test("an admin has no unlink action after a usage error", async () => {
    const { transport } = usageFailingTransport(
      createMockTransport({ userId: ADMIN }),
    );
    renderConsole(SETTINGS, { transport });
    const section = await billing();
    await within(section).findByRole("button", { name: "Try again" });
    expect(
      within(section).queryByRole("button", { name: "Unlink billing account" }),
    ).toBeNull();
  });
});
