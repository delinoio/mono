// SPDX-License-Identifier: Apache-2.0
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type ListSandboxesResponse,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { QueryClient } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { invalidateServices, POLL_INTERVAL_MS } from "./api.ts";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { TEST_SETUP_MS, renderConsole } from "./test-support.tsx";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  rs.useRealTimers();
  // The toaster is module state outside the rendered tree.
  toast.dismiss();
});

/** The error notices of the page (the toaster region also has an empty alert). */
const notices = () => document.querySelectorAll(".notice-error");
const FAILED_TEXT = "Something went wrong. Try again.";

/** The sandbox rows, read from the DOM (a role query over 50 rows is slow). */
const rows = () => document.querySelectorAll("tbody tr");

/**
 * Records the `page_token` of every `ListSandboxes` request. While
 * `state.failLater` is set, every request that has a token (every page after
 * the first one) fails with `state.code`; `state.failFirst` fails the request
 * without a token instead.
 */
function failingTransport(firstRowState?: SandboxState) {
  const mock = createMockTransport({ extraSandboxes: 48 });
  const state = {
    tokens: [] as string[],
    failLater: false,
    failFirst: false,
    code: Code.Internal,
    firstRowState,
  };
  const transport: Transport = {
    ...mock,
    async unary(m, ...rest) {
      if (m.name !== "ListSandboxes") return mock.unary(m, ...rest);

      const token = (rest[3] as { pageToken: string }).pageToken;
      state.tokens.push(token);
      if (token === "" ? state.failFirst : state.failLater) {
        throw new ConnectError("the page failed", state.code);
      }
      const response = await mock.unary(m, ...rest);
      if (token === "" && state.firstRowState !== undefined) {
        const { sandboxes } = response.message as ListSandboxesResponse;
        sandboxes[0]!.state = state.firstRowState;
      }
      return response;
    },
  };
  return { transport, state };
}

/** Opens the list on its full first page and clicks Show more while the next page fails. */
async function showMoreThatFails(
  code = Code.Internal,
  firstRowState?: SandboxState,
) {
  const { transport, state } = failingTransport(firstRowState);
  renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
  const more = await screen.findByRole("button", { name: "Show more" }, WAIT);
  expect(rows()).toHaveLength(50);
  state.failLater = true;
  state.code = code;
  await act(async () => {
    fireEvent.click(more);
  });
  return state;
}

describe("sandboxes page when a Show more request fails", () => {
  for (const settling of [SandboxState.CREATING, SandboxState.DELETING]) {
    test(
      `pauses polling while a ${SandboxState[settling]} row has a continuation error`,
      async () => {
        rs.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
        const state = await showMoreThatFails(Code.Internal, settling);

        await waitFor(() => expect(notices()).toHaveLength(1), WAIT);
        const failedToken = state.tokens[1];
        expect(failedToken).toBeTruthy();
        await act(async () => {
          rs.advanceTimersByTime(2 * POLL_INTERVAL_MS);
        });
        expect(state.tokens).toEqual(["", failedToken]);
        expect(notices()[0]?.textContent).toContain(FAILED_TEXT);
        expect(notices()[0]?.textContent).not.toContain("refresh");
        expect(rows()).toHaveLength(50);

        state.failLater = false;
        const retry = screen.getByRole("button", { name: "Try again" });
        const more = screen.getByRole("button", { name: "Show more" });
        await act(async () => {
          fireEvent.click(retry);
          fireEvent.click(retry);
          fireEvent.click(more);
        });
        await waitFor(() => expect(rows()).toHaveLength(51), WAIT);
        expect(notices()).toHaveLength(0);
        expect(state.tokens).toEqual(["", failedToken, failedToken]);
        expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();

        state.firstRowState = SandboxState.RUNNING;
        await act(async () => {
          rs.advanceTimersByTime(POLL_INTERVAL_MS);
        });
        const afterPoll = ["", failedToken, failedToken, "", failedToken];
        expect(state.tokens).toEqual(afterPoll);
        await waitFor(
          () => expect(rows()[0]?.textContent).toContain("Running"),
          WAIT,
        );
        await act(async () => {
          rs.advanceTimersByTime(POLL_INTERVAL_MS);
        });
        expect(state.tokens).toEqual(afterPoll);
        expect(rows()).toHaveLength(51);
      },
      4 * WAIT.timeout + TEST_SETUP_MS,
    );
  }

  test(
    "a failed retry keeps the error and the rows",
    async () => {
      const state = await showMoreThatFails();
      await waitFor(() => expect(notices()).toHaveLength(1), WAIT);
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      });
      await waitFor(() => expect(state.tokens).toHaveLength(3), WAIT);
      await waitFor(() => expect(notices()).toHaveLength(1), WAIT);
      expect(notices()[0]?.textContent).toContain(FAILED_TEXT);
      expect(rows()).toHaveLength(50);
      expect(state.tokens[2]).toBe(state.tokens[1]);
    },
    4 * WAIT.timeout + TEST_SETUP_MS,
  );

  for (const [code, message] of [
    [Code.NotFound, "It was not found, or you no longer have access to it."],
    [Code.PermissionDenied, "You do not have permission to do this."],
    [Code.Unauthenticated, "Your session ended. Sign in again."],
  ] as const) {
    test(
      `hides the list and Show more after ${Code[code]}`,
      async () => {
        const state = await showMoreThatFails(code);
        await screen.findByText(message, {}, WAIT);
        expect(rows()).toHaveLength(0);
        expect(notices()).toHaveLength(1);
        expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();

        if (code === Code.PermissionDenied) {
          // The full error offers a list refresh starting at the first page.
          await act(async () => {
            fireEvent.click(screen.getByRole("button", { name: "Try again" }));
          });
          await waitFor(() => expect(rows()).toHaveLength(50), WAIT);
          const failedToken = state.tokens[1];
          expect(failedToken).toBeTruthy();
          expect(state.tokens).toEqual(["", failedToken, ""]);
        } else {
          expect(
            screen.queryByRole("button", { name: "Try again" }),
          ).toBeNull();
        }
        if (code === Code.Unauthenticated) {
          expect(
            screen.getByRole("button", { name: "Sign in with Delino" }),
          ).toBeTruthy();
        }
      },
      (code === Code.PermissionDenied ? 3 : 2) * WAIT.timeout + TEST_SETUP_MS,
    );
  }
});

describe("sandboxes page when the first page fails", () => {
  test(
    "shows the initial error and its retry reads the first page again",
    async () => {
      const { transport, state } = failingTransport();
      state.failFirst = true;
      renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
      await waitFor(() => expect(notices()).toHaveLength(1), WAIT);
      expect(notices()[0]?.textContent).toContain(FAILED_TEXT);
      expect(rows()).toHaveLength(0);
      expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();

      state.failFirst = false;
      const before = state.tokens.length;
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      });
      await screen.findByRole("button", { name: "Show more" }, WAIT);
      expect(rows()).toHaveLength(50);
      expect(state.tokens.slice(before)).toEqual([""]);
    },
    2 * WAIT.timeout + TEST_SETUP_MS,
  );
});

describe("sandboxes page when a list refresh fails", () => {
  for (const code of [Code.PermissionDenied, Code.Internal]) {
    test(
      `handles the loaded list and Show more after ${Code[code]}`,
      async () => {
        const queryClient = new QueryClient({
          defaultOptions: { queries: { retry: false } },
        });
        const { transport, state } = failingTransport();
        renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
          queryClient,
          transport,
        });
        await screen.findByRole("button", { name: "Show more" }, WAIT);
        expect(rows()).toHaveLength(50);
        expect(state.tokens).toEqual([""]);

        state.failFirst = true;
        state.code = code;
        await act(async () => {
          await invalidateServices(queryClient, SandboxService);
        });
        await waitFor(() => expect(notices()).toHaveLength(1), WAIT);
        expect(state.tokens).toEqual(["", ""]);

        if (code === Code.Internal) {
          expect(notices()[0]?.textContent).toContain(
            "The last refresh failed",
          );
          expect(rows()).toHaveLength(50);
          expect(
            screen.getByRole("button", { name: "Show more" }),
          ).toBeTruthy();
          return;
        }

        expect(notices()[0]?.textContent).toContain(
          "You do not have permission to do this.",
        );
        expect(rows()).toHaveLength(0);
        expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
        state.failFirst = false;
        await act(async () => {
          fireEvent.click(screen.getByRole("button", { name: "Try again" }));
        });
        await screen.findByRole("button", { name: "Show more" }, WAIT);
        expect(rows()).toHaveLength(50);
        expect(state.tokens).toEqual(["", "", ""]);
      },
      (code === Code.Internal ? 2 : 3) * WAIT.timeout + TEST_SETUP_MS,
    );
  }
});
