// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
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

/** The sandbox rows, read from the DOM (a role query over 50 rows is slow). */
const rows = () => document.querySelectorAll("tbody tr");

describe("sandboxes page against the mock paging", () => {
  test("Show more adds the last sandbox of the mock and then disappears", async () => {
    // 3 seeded live sandboxes and 48 extra ones make 51 matching rows: one
    // full page of 50 and one more.
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport: createMockTransport({ extraSandboxes: 48 }),
    });
    const more = await screen.findByRole("button", { name: "Show more" }, WAIT);
    expect(rows()).toHaveLength(50);
    await act(async () => {
      fireEvent.click(more);
    });
    await waitFor(() => expect(rows()).toHaveLength(51), WAIT);
    await waitFor(
      () =>
        expect(screen.queryByRole("button", { name: "Show more" })).toBeNull(),
      WAIT,
    );
  });

  test("offers no Show more for fewer than 50 sandboxes", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`);
    await waitFor(() => expect(rows()).toHaveLength(3), WAIT);
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
  });
});
