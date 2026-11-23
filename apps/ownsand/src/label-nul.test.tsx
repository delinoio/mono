// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { parseLabels } from "./forms.ts";
import { MOCK_IDS } from "./mock/mock-api.ts";
import { holdingTransport, renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

describe("parseLabels and U+0000", () => {
  test("rejects U+0000 in a key", () => {
    expect(parseLabels("a\u0000b=c")).toEqual({
      error: "A label key must not contain a NUL character.",
    });
  });

  test("rejects U+0000 in a value", () => {
    expect(parseLabels("a=b\u0000c")).toEqual({
      error: "A label value must not contain a NUL character.",
    });
    expect(parseLabels("a=\u0000")).toHaveProperty("error");
  });

  test("rejects U+0000 on a later line", () => {
    expect(parseLabels("team=ci\na=b\u0000")).toHaveProperty("error");
  });

  test("does not echo U+0000 in the malformed-line message", () => {
    const parsed = parseLabels("no\u0000value");
    expect(parsed).toHaveProperty("error");
    expect((parsed as { error: string }).error).not.toContain("\u0000");
  });

  test("keeps the label once U+0000 is removed", () => {
    expect(parseLabels("a=bc")).toEqual({ labels: { a: "bc" } });
  });
});

describe("create sandbox labels with U+0000", () => {
  async function openForm() {
    const held = holdingTransport(["CreateSandbox"], () => false);
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport: held.transport,
    });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("button", { name: "Create sandbox" }),
      );
    });
    await settle(60);
    return {
      held,
      dialog: await screen.findByRole("dialog", { name: "Create a sandbox" }),
    };
  }

  const type = async (dialog: HTMLElement, value: string) => {
    await act(async () => {
      fireEvent.input(within(dialog).getByRole("textbox", { name: "Labels" }), {
        target: { value },
      });
    });
  };

  const create = async (dialog: HTMLElement) => {
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    });
    await settle(60);
  };

  test.each(["a\u0000b=c", "a=b\u0000c"])(
    "shows a Labels error and sends no request for %j, then submits the corrected label",
    async (text) => {
      const { held, dialog } = await openForm();
      await type(dialog, text);
      await create(dialog);
      expect(held.seen.CreateSandbox ?? 0).toBe(0);
      const field = within(dialog).getByRole("textbox", { name: "Labels" });
      expect(field.getAttribute("aria-invalid")).toBe("true");
      expect(dialog.textContent).toContain("must not contain a NUL character.");
      expect(
        screen.queryByRole("dialog", { name: "Create a sandbox" }),
      ).not.toBeNull();
      await type(dialog, "a=bc");
      await create(dialog);
      expect(held.seen.CreateSandbox).toBe(1);
      expect(window.location.pathname).toMatch(/\/sandboxes\/[0-9a-f-]{36}$/);
    },
  );
});
