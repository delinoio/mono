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
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";
import {
  type CreateSandboxRequest,
  type CreateSandboxResponse,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { MOCK_IDS } from "./mock/mock-api.ts";
import { holdingTransport, renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

const SANDBOXES = `/orgs/${MOCK_IDS.acme}/sandboxes`;
const RESERVATION_MS = 24 * 60 * 60 * 1000;

async function openForm(transport: Transport) {
  renderConsole(SANDBOXES, { transport });
  await act(async () => {
    fireEvent.click(
      await screen.findByRole("button", { name: "Create sandbox" }),
    );
  });
  return within(
    await screen.findByRole("dialog", { name: "Create a sandbox" }),
  );
}

async function submit(dialog: ReturnType<typeof within>, name: string) {
  await act(async () => {
    fireEvent.click(dialog.getByRole("button", { name }));
  });
}

function loseCreateReplies(replayError?: Code) {
  const { transport: base } = holdingTransport([]);
  const requests: CreateSandboxRequest[] = [];
  const sandboxIds: string[] = [];
  const transport: Transport = {
    ...base,
    async unary(method, ...args) {
      if (method.name !== "CreateSandbox") return base.unary(method, ...args);
      requests.push(args[3] as CreateSandboxRequest);
      if (requests.length === 2 && replayError !== undefined) {
        throw new ConnectError("Replay failed", replayError);
      }
      const response = await base.unary(method, ...args);
      const { sandbox } = response.message as unknown as CreateSandboxResponse;
      sandboxIds.push(sandbox!.sandboxId);
      if (requests.length === 1) {
        throw new ConnectError("Response lost", Code.Unavailable);
      }
      return response;
    },
  };
  return { transport, base, requests, sandboxIds };
}

describe("create sandbox navigation", () => {
  test("opens the new sandbox without waiting for the list refetch", async () => {
    // The page's first load of the list passes; the refetch that the
    // invalidation after the create starts is held.
    let hold = false;
    const { transport, calls } = holdingTransport(
      ["ListSandboxes"],
      () => hold,
    );
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Create sandbox" }),
      );
    });
    await settle(60);
    const dialog = await screen.findByRole("dialog", {
      name: "Create a sandbox",
    });
    hold = true;
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
    // The list refetch is still held, so the invalidation did run, and the
    // console already moved on to the new sandbox.
    expect(calls.ListSandboxes).toHaveLength(1);
    expect(window.location.pathname).toMatch(
      new RegExp(`^/orgs/${MOCK_IDS.acme}/sandboxes/[0-9a-f-]{36}$`),
    );
    expect(
      screen.queryByRole("dialog", { name: "Create a sandbox" }),
    ).toBeNull();
    expect(
      await screen.findByRole("heading", { name: "Sandbox" }),
    ).toBeTruthy();
    await act(async () => {
      calls.ListSandboxes?.[0]?.settle();
    });
    await settle(60);
    expect(window.location.pathname).toMatch(/\/sandboxes\/[0-9a-f-]{36}$/);
  });

  test("recovers a committed create with the full original request after losing its reply", async () => {
    const { transport, base, requests, sandboxIds } = loseCreateReplies();
    const dialog = await openForm(transport);
    const labels = dialog.getByLabelText<HTMLTextAreaElement>("Labels");
    const vcpus = dialog.getByLabelText<HTMLInputElement>("vCPUs");
    await act(async () => {
      fireEvent.input(labels, { target: { value: "team=recover" } });
    });
    const form = labels.closest("form")!;
    const submitTwice = async () => {
      await act(async () => {
        fireEvent.submit(form);
        fireEvent.submit(form);
      });
    };
    await submitTwice();

    expect(
      await dialog.findByText(/The sandbox may have been created/),
    ).toBeTruthy();
    expect(window.location.pathname).toBe(SANDBOXES);
    expect(labels.disabled).toBe(true);
    expect(labels.value).toBe("team=recover");
    expect(vcpus.disabled).toBe(true);
    expect(dialog.queryByRole("button", { name: "Create" })).toBeNull();

    await submitTwice();
    expect(requests).toEqual([requests[0], requests[0]]);
    expect(requests[0]?.labels).toEqual({ team: "recover" });
    expect(sandboxIds).toEqual([sandboxIds[0], sandboxIds[0]]);
    expect(window.location.pathname).toBe(`${SANDBOXES}/${sandboxIds[0]}`);
    const list = await createClient(SandboxService, base).listSandboxes({
      organizationId: MOCK_IDS.acme,
    });
    expect(
      list.sandboxes.filter((sandbox) => sandbox.labels.team === "recover"),
    ).toHaveLength(1);
  });

  for (const replayError of [Code.Unavailable, Code.InvalidArgument]) {
    test(`keeps the original deadline after replay returns ${Code[replayError]}`, async () => {
      const start = Date.now();
      const now = rs.spyOn(Date, "now").mockReturnValue(start);
      try {
        const { transport, requests } = loseCreateReplies(replayError);
        const dialog = await openForm(transport);
        await submit(dialog, "Create");

        now.mockReturnValue(start + RESERVATION_MS - 1);
        await submit(dialog, "Retry original request");
        expect(requests).toEqual([requests[0], requests[0]]);
        expect(dialog.getByLabelText<HTMLInputElement>("vCPUs").disabled).toBe(
          true,
        );
        const retry = dialog.getByRole<HTMLButtonElement>("button", {
          name: "Retry original request",
        });
        expect(retry.disabled).toBe(false);

        // The UI timer has not run. The submit handler must enforce expiry.
        now.mockReturnValue(start + RESERVATION_MS);
        await submit(dialog, "Retry original request");
        expect(requests).toHaveLength(2);
        expect(retry.disabled).toBe(true);
        expect(dialog.getByText(/Check the sandbox list/)).toBeTruthy();
        expect(window.location.pathname).toBe(SANDBOXES);
      } finally {
        now.mockRestore();
      }
    });
  }

  test("allows editing and resubmitting after a definitive first rejection", async () => {
    const { transport: base } = holdingTransport([]);
    const requests: CreateSandboxRequest[] = [];
    const dialog = await openForm({
      ...base,
      async unary(method, ...args) {
        if (method.name === "CreateSandbox") {
          requests.push(args[3] as CreateSandboxRequest);
          if (requests.length === 1) {
            throw new ConnectError(
              "Change the team label.",
              Code.InvalidArgument,
            );
          }
        }
        return base.unary(method, ...args);
      },
    });

    await submit(dialog, "Create");
    expect(await dialog.findByText("Change the team label.")).toBeTruthy();
    const labels = dialog.getByLabelText<HTMLTextAreaElement>("Labels");
    expect(labels.disabled).toBe(false);

    await act(async () => {
      fireEvent.input(labels, { target: { value: "team=corrected" } });
    });
    await submit(dialog, "Create");
    expect(requests).toHaveLength(2);
    expect(requests[1]?.idempotencyKey).not.toBe(requests[0]?.idempotencyKey);
    expect(requests[1]?.labels).toEqual({ team: "corrected" });
    expect(window.location.pathname).toMatch(/\/sandboxes\/[0-9a-f-]{36}$/);
  });

  test("rejects a malformed ID and blocks replay at the reservation deadline", async () => {
    const start = Date.now();
    const now = rs.spyOn(Date, "now").mockReturnValue(start);
    try {
      const { transport, seen } = holdingTransport([]);
      renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
        transport: {
          ...transport,
          async unary(method, ...args) {
            const response = await transport.unary(method, ...args);
            if (method.name !== "CreateSandbox") return response;
            const { sandbox } =
              response.message as unknown as CreateSandboxResponse;
            return {
              ...response,
              message: {
                ...response.message,
                sandbox: { ...sandbox, sandboxId: "../devices" },
              },
            };
          },
        },
      });
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("button", { name: "Create sandbox" }),
        );
      });
      const dialog = await screen.findByRole("dialog", {
        name: "Create a sandbox",
      });
      await act(async () => {
        fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
      });
      expect(
        await within(dialog).findByText(/The sandbox may have been created/),
      ).toBeTruthy();
      expect(window.location.pathname).toBe(`/orgs/${MOCK_IDS.acme}/sandboxes`);

      const retry = within(dialog).getByRole<HTMLButtonElement>("button", {
        name: "Retry original request",
      });
      expect(retry.disabled).toBe(false);
      // The click must check the deadline even if the browser has not run
      // the timer that disables the button yet.
      now.mockReturnValue(start + RESERVATION_MS);
      await act(async () => {
        fireEvent.click(retry);
      });
      expect(retry.disabled).toBe(true);
      expect(
        within(dialog).getByText(/The retry window has ended/),
      ).toBeTruthy();
      expect(seen.CreateSandbox).toBe(1);
    } finally {
      now.mockRestore();
    }
  });
});
