// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { renderConsole } from "./test-support.tsx";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
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
function failingTransport() {
  const mock = createMockTransport({ extraSandboxes: 48 });
  const state = {
    tokens: [] as string[],
    failLater: false,
    failFirst: false,
    code: Code.Internal,
  };
  const transport: Transport = {
    ...mock,
    async unary(m, ...rest) {
      if (m.name === "ListSandboxes") {
        const token = (rest[3] as { pageToken: string }).pageToken;
        state.tokens.push(token);
        if (token === "" ? state.failFirst : state.failLater) {
          throw new ConnectError("the page failed", state.code);
        }
      }
      return mock.unary(m, ...rest);
    },
  };
  return { transport, state };
}

/** Opens the list on its full first page and clicks Show more while the next page fails. */
async function showMoreThatFails(code = Code.Internal) {
  const { transport, state } = failingTransport();
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
  test("keeps the rows, shows the error next to Show more and retries the failed token", async () => {
    const state = await showMoreThatFails();

    await waitFor(() => expect(notices()).toHaveLength(1), WAIT);
    expect(notices()[0]?.textContent).toContain(FAILED_TEXT);
    // It is a pagination error, not the notice of a failed refresh.
    expect(notices()[0]?.textContent).not.toContain("refresh");
    expect(screen.getAllByRole("button", { name: "Try again" })).toHaveLength(
      1,
    );
    expect(rows()).toHaveLength(50);
    expect(screen.getByRole("button", { name: "Show more" })).toBeTruthy();
    const failedToken = state.tokens[1];
    expect(failedToken).not.toBe("");
    expect(state.tokens).toEqual(["", failedToken]);

    // Retry and Show more share the same pending continuation.
    state.failLater = false;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      fireEvent.click(screen.getByRole("button", { name: "Show more" }));
    });
    await waitFor(() => expect(rows()).toHaveLength(51), WAIT);
    await waitFor(() => expect(notices()).toHaveLength(0), WAIT);
    expect(state.tokens).toEqual(["", failedToken, failedToken]);
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
    expect(rows()).toHaveLength(51);
  });

  test("a failed retry keeps the error and the rows", async () => {
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
  });

  test("a terminal error of the next page takes the full error path", async () => {
    await showMoreThatFails(Code.PermissionDenied);
    // The rows are replaced by the notice, as for every terminal error.
    await waitFor(() => expect(rows()).toHaveLength(0), WAIT);
    await waitFor(() => expect(notices()).toHaveLength(1), WAIT);
    expect(notices()[0]?.textContent).toContain(
      "You do not have permission to do this.",
    );
  });
});

describe("sandboxes page when the first page fails", () => {
  test("shows the initial error and its retry reads the first page again", async () => {
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
  });
});
