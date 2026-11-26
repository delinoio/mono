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
  BillingService,
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
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { holdingTransport, renderConsole } from "./test-support.tsx";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };

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
  test("the owner can unlink while the usage is loading", async () => {
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
  });

  test("the owner can open the confirmation after a usage error, and the retry stays", async () => {
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
    await within(section).findByText("Acme Robotics billing", undefined, WAIT);
    expect(
      within(section).getByRole("button", { name: "Unlink billing account" }),
    ).toBeTruthy();
    expect(
      within(section).queryByRole("button", { name: "Try again" }),
    ).toBeNull();
  });

  test("confirming after a usage error unlinks once and shows the unlinked view", async () => {
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
      within(section).queryByRole("button", { name: "Unlink billing account" }),
    ).toBeNull();
    expect(
      within(section).queryByRole("button", { name: "Try again" }),
    ).toBeNull();
  });

  test("a stale confirmation keeps its original target and requires a new opening", async () => {
    const mock = createMockTransport();
    const client = createClient(BillingService, mock);
    const organizationId = MOCK_IDS.acme;
    const { billingAccountId: accountA } = await client.getBillingStatus({
      organizationId,
    });
    const accounts = await client.listLinkableBillingAccounts({
      organizationId,
    });
    const accountB = accounts.billingAccounts.find(
      (account) => account.billingAccountId !== accountA,
    )!;
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const requestedIds: (string | undefined)[] = [];
    let statusCalls = 0;
    let failUsage = false;
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name === "GetBillingStatus") statusCalls++;
        if (method.name === "GetBillingUsage" && failUsage) {
          throw new ConnectError("usage is unavailable", Code.Unavailable);
        }
        if (method.name === "UnlinkBillingAccount") {
          requestedIds.push(
            (rest[3] as UnlinkBillingAccountRequest).expectedBillingAccountId,
          );
        }
        return mock.unary(method, ...rest);
      },
    };
    renderConsole(SETTINGS, { transport, queryClient });
    const section = await billing();
    await within(section).findByText("Acme Robotics billing", undefined, WAIT);
    const unlink = () =>
      within(section).getByRole("button", { name: "Unlink billing account" });
    const confirm = (dialog: HTMLElement) =>
      click(
        within(dialog).getByRole("button", { name: "Unlink billing account" }),
      );

    await click(unlink());
    const opening = await screen.findByRole("alertdialog", {
      name: "Unlink Acme Robotics billing?",
    });
    await client.unlinkBillingAccount({ organizationId });
    await client.linkBillingAccount({
      organizationId,
      billingAccountId: accountB.billingAccountId,
    });
    await act(async () => {
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({
          schema: BillingService.method.getBillingStatus,
          cardinality: undefined,
        }),
      });
    });
    expect(within(section).getByText("Acme Robotics billing")).toBeTruthy();
    expect(
      within(opening).getByRole("heading", {
        name: "Unlink Acme Robotics billing?",
      }),
    ).toBeTruthy();

    failUsage = true;
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
    ).toBe(accountB.billingAccountId);

    await click(unlink());
    const newOpening = await screen.findByRole("alertdialog", {
      name: "Unlink the billing account?",
    });
    await confirm(newOpening);
    await within(section).findByRole(
      "button",
      { name: "Link a billing account" },
      WAIT,
    );
    expect(requestedIds).toEqual([accountA, accountB.billingAccountId]);
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

  test("a failed unlink keeps the confirmation open with the error", async () => {
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
      { timeout: 3000 },
    );
    expect(
      within(section).queryByRole("button", { name: "Link a billing account" }),
    ).toBeNull();
  });

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
