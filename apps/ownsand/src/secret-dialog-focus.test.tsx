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
import { MOCK_IDS } from "./mock/mock-api.ts";
import { renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  rs.restoreAllMocks();
});

async function click(element: HTMLElement) {
  await act(async () => {
    element.focus();
    fireEvent.click(element);
  });
  await settle(60);
}

interface Flow {
  name: string;
  path: string;
  /** Opens the secret dialog and returns its title. */
  open: () => Promise<string>;
}

const FLOWS: Flow[] = [
  {
    name: "enrollment token",
    path: `/orgs/${MOCK_IDS.acme}/enrollment-tokens`,
    open: async () => {
      await click(screen.getByRole("button", { name: "Create token" }));
      await click(
        within(screen.getByRole("dialog")).getByRole("button", {
          name: "Create token",
        }),
      );
      return "Your new enrollment token";
    },
  },
  {
    name: "API key",
    path: `/orgs/${MOCK_IDS.acme}/api-keys`,
    open: async () => {
      await click(screen.getByRole("button", { name: "Create API key" }));
      const form = screen.getByRole("dialog");
      await act(async () => {
        fireEvent.input(within(form).getByRole("textbox", { name: "Name" }), {
          target: { value: "focus-key" },
        });
      });
      await click(within(form).getByRole("button", { name: "Create key" }));
      return "Your new API key";
    },
  },
  {
    name: "invitation link",
    path: `/orgs/${MOCK_IDS.acme}/members`,
    open: async () => {
      await click(screen.getByRole("button", { name: "Invite someone" }));
      await click(
        within(screen.getByRole("dialog")).getByRole("button", {
          name: "Create link",
        }),
      );
      return "Your invitation link";
    },
  },
];

describe.each(FLOWS)("$name secret dialog", (flow) => {
  async function start() {
    renderConsole(flow.path);
    await screen.findByRole("heading", { level: 1 });
    await settle(60);
    const title = await flow.open();
    const dialog = await screen.findByRole("dialog", { name: title });
    await settle(60);
    return dialog;
  }

  test("opens with the focus on Copy, and Done stays first in the DOM", async () => {
    const dialog = await start();
    const copy = within(dialog).getByRole("button", { name: "Copy" });
    const done = within(dialog).getByRole("button", { name: "Done" });
    expect(document.activeElement).toBe(copy);
    expect(
      done.compareDocumentPosition(copy) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  test("activating Copy copies the secret and keeps the dialog open", async () => {
    const written: string[] = [];
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: async (text: string) => {
          written.push(text);
        },
      },
    });
    const dialog = await start();
    const secret = within(dialog).getByLabelText(/./, {
      selector: "code",
    }).textContent;
    const copy = within(dialog).getByRole("button", { name: "Copy" });
    await click(copy);
    expect(written).toEqual([secret]);
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(document.activeElement).toBe(copy);
  });

  test("Done closes the dialog", async () => {
    const dialog = await start();
    await click(within(dialog).getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("Escape closes the dialog", async () => {
    await start();
    await act(async () => {
      fireEvent.keyDown(document.activeElement as Element, { key: "Escape" });
    });
    await settle(60);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
