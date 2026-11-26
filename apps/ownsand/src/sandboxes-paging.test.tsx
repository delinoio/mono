// SPDX-License-Identifier: Apache-2.0
import { SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import type {
  ListSandboxesRequest,
  ListSandboxesResponse,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import type { Transport } from "@connectrpc/connect";
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
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { POLL_INTERVAL_MS } from "./api.ts";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { TEST_SETUP_MS, renderConsole, settle } from "./test-support.tsx";

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

/** The sandbox rows, read from the DOM (a role query over 50 rows is slow). */
const rows = () => document.querySelectorAll("tbody tr");

function sandboxPagingTransport({ settling = false } = {}) {
  // The mock has 3 live sandboxes and one ended sandbox before adding extras.
  const mock = createMockTransport({ extraSandboxes: 48 });
  const calls: {
    input: ListSandboxesRequest;
    signal: AbortSignal | undefined;
    release: () => void;
  }[] = [];
  const transport: Transport = {
    ...mock,
    async unary(method, ...args) {
      if (method.name !== "ListSandboxes") return mock.unary(method, ...args);

      const input = args[3] as ListSandboxesRequest;
      const hold = Boolean(input.pageToken) || (settling && calls.length > 0);
      const held = new Promise<void>((release) => {
        calls.push({ input, signal: args[0], release });
      });

      const response = await mock.unary(method, ...args);
      if (settling && !input.pageToken) {
        const message = response.message as ListSandboxesResponse;
        const first = message.sandboxes[0]!;
        message.sandboxes = [
          { ...first, state: SandboxState.CREATING },
          ...message.sandboxes.slice(1),
        ];
      }
      // Return the response even if the query cancels while the fixture holds it.
      if (hold) await held;
      return response;
    },
  };
  return { transport, calls };
}

describe("sandboxes page against the mock paging", () => {
  test(
    "Show more supersedes a poll and appends the final row once",
    async () => {
      rs.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
      const { transport, calls } = sandboxPagingTransport({ settling: true });
      renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });

      const more = await screen.findByRole(
        "button",
        { name: "Show more" },
        WAIT,
      );
      expect(rows()).toHaveLength(50);
      await act(async () => {
        rs.advanceTimersByTime(POLL_INTERVAL_MS);
      });
      expect(calls.map(({ input }) => input.pageToken)).toEqual(["", ""]);

      await act(async () => {
        fireEvent.click(more);
        fireEvent.click(more);
      });
      expect(calls[1]?.signal?.aborted).toBe(true);
      const token = calls[2]?.input.pageToken;
      expect(token).toBeTruthy();

      await act(async () => {
        rs.advanceTimersByTime(POLL_INTERVAL_MS);
      });
      expect(calls.map(({ input }) => input.pageToken)).toEqual([
        "",
        "",
        token,
      ]);

      calls[2]!.release();
      await waitFor(() => expect(rows()).toHaveLength(51), WAIT);
      expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();

      calls[1]!.release();
      await settle();
      expect(rows()).toHaveLength(51);
      expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
    },
    2 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "loads a continuation for a new filter while the old response is held",
    async () => {
      const { transport, calls } = sandboxPagingTransport();
      renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });

      const more = await screen.findByRole(
        "button",
        { name: "Show more" },
        WAIT,
      );
      await act(async () => {
        fireEvent.click(more);
      });
      const oldPage = calls[1]!;
      expect(oldPage.input.includeEnded).toBe(false);
      expect(oldPage.input.pageToken).toBeTruthy();

      await act(async () => {
        fireEvent.click(
          screen.getByRole("checkbox", { name: "Show ended sandboxes" }),
        );
      });
      const filteredMore = await screen.findByRole(
        "button",
        { name: "Show more" },
        WAIT,
      );
      await act(async () => {
        fireEvent.click(filteredMore);
        fireEvent.click(filteredMore);
      });
      const filteredPage = calls[3]!;
      expect(calls).toHaveLength(4);
      expect(calls[2]!.input).toMatchObject({
        includeEnded: true,
        pageToken: "",
      });
      expect(filteredPage.input.includeEnded).toBe(true);
      expect(filteredPage.input.pageToken).toBeTruthy();

      oldPage.release();
      await settle();
      expect(rows()).toHaveLength(50);
      expect((filteredMore as HTMLButtonElement).disabled).toBe(true);
      filteredPage.release();
      await waitFor(() => expect(rows()).toHaveLength(52), WAIT);
      expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
    },
    3 * WAIT.timeout + TEST_SETUP_MS,
  );

  test(
    "offers no Show more for fewer than 50 sandboxes",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`);
      await waitFor(() => expect(rows()).toHaveLength(3), WAIT);
      expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
    },
    WAIT.timeout + TEST_SETUP_MS,
  );
});
