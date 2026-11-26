// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
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
  focusPageHeading,
  focusPageHeadingAfterNavigation,
} from "./components/focus.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MockBillingState,
} from "./mock/mock-api.ts";
import {
  DEFAULT_WAIT_MS,
  TEST_SETUP_MS,
  renderConsole,
  settle,
} from "./test-support.tsx";

/** The longest wait for one condition; a loaded CI runner needs more than one second. */
const WAIT = { timeout: 10_000 };

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  toast.dismiss();
  document.body.replaceChildren();
});

/** Focuses the opener and clicks it, as a keyboard user does. */
async function press(opener: HTMLElement) {
  await act(async () => {
    opener.focus();
    fireEvent.click(opener);
  });
  await settle(30);
}

async function confirm(name: string) {
  const dialog = await screen.findByRole("alertdialog");
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name }));
  });
}

/** Waits until the page heading `title` holds the focus. */
async function expectHeadingFocused(title: string) {
  await waitFor(() => {
    const heading = screen.getByRole("heading", { level: 1, name: title });
    expect(document.activeElement).toBe(heading);
  }, WAIT);
}

describe("focus after a confirmed action removed the opener", () => {
  test(
    "Remove member",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/members`);
      const row = (await screen.findByText("user-member-ada")).closest("li")!;
      await press(within(row).getByRole("button", { name: /^Remove/ }));
      await confirm("Remove member");
      await expectHeadingFocused("Members");
      expect(screen.queryByText("user-member-ada")).toBeNull();
    },
    WAIT.timeout + 2 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "Revoke invitation",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/members`);
      const list = await screen.findByRole("list", {
        name: "Pending invitations",
      });
      await press(within(list).getByRole("button", { name: /^Revoke/ }));
      await confirm("Revoke invitation");
      await expectHeadingFocused("Members");
      expect(await screen.findByText("No pending invitations")).toBeTruthy();
    },
    WAIT.timeout + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "Revoke API key",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/api-keys`);
      await screen.findByText(/ci-runner/);
      await press(screen.getByRole("button", { name: /^Revoke/ }));
      await confirm("Revoke key");
      await expectHeadingFocused("API keys");
      expect(screen.queryByRole("button", { name: /^Revoke/ })).toBeNull();
    },
    WAIT.timeout + 2 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  // React reuses the DOM node of the unlink button as the link button (and
  // the other way round), so these two openers still exist after the action:
  // the SDK returns the focus to that node and the fallback leaves it alone.
  // The user's keyboard position stays on the billing action.
  test(
    "Unlink billing account keeps the focus on the reused button",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`);
      const section = await screen.findByRole("region", { name: "Billing" });
      await press(
        await within(section).findByRole("button", {
          name: "Unlink billing account",
        }),
      );
      await confirm("Unlink billing account");
      const link = await within(section).findByRole("button", {
        name: "Link a billing account",
      });
      await waitFor(() => expect(document.activeElement).toBe(link), WAIT);
    },
    WAIT.timeout + 4 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "Link a billing account keeps the focus on the reused button",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`, {
        transport: createMockTransport({
          billing: { [MOCK_IDS.acme]: MockBillingState.BlockedUnlinked },
        }),
      });
      const section = await screen.findByRole("region", { name: "Billing" });
      await press(
        await within(section).findByRole("button", {
          name: "Link a billing account",
        }),
      );
      const dialog = await screen.findByRole("dialog", {
        name: "Link a billing account",
      });
      const [first] = await within(dialog).findAllByRole("radio");
      await act(async () => {
        fireEvent.click(first!);
      });
      await act(async () => {
        fireEvent.click(within(dialog).getByRole("button", { name: "Link" }));
      });
      const unlink = await within(section).findByRole("button", {
        name: "Unlink billing account",
      });
      await waitFor(() => expect(document.activeElement).toBe(unlink), WAIT);
    },
    WAIT.timeout + 5 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "Transfer ownership",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`);
      await screen.findByRole("region", { name: "Billing" });
      await settle(60);
      await act(async () => {
        fireEvent.click(screen.getByRole("combobox", { name: "New owner" }));
      });
      await act(async () => {
        fireEvent.click(
          await screen.findByRole("option", { name: "Jun Park" }),
        );
      });
      await press(screen.getByRole("button", { name: "Transfer" }));
      await confirm("Transfer ownership");
      await waitFor(
        () =>
          expect(screen.queryByRole("button", { name: "Transfer" })).toBeNull(),
        WAIT,
      );
      // The whole owner section, with its dialog, unmounts. A real browser
      // finishes the closing of the dialog afterwards and reaches the heading
      // (checked in Chrome by the e2e suite); happy-dom drops the dialog
      // without the completion callback, so only the page body is asserted
      // not to hold a removed element.
      expect(document.activeElement?.isConnected).toBe(true);
    },
    WAIT.timeout + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );
});

describe("focus after a navigation that removed the opener", () => {
  test(
    "Delete organization ends on the Organizations heading",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/settings`);
      await press(
        await screen.findByRole("button", { name: "Delete organization" }),
      );
      await confirm("Delete organization");
      await waitFor(() => expect(window.location.pathname).toBe("/"), WAIT);
      await expectHeadingFocused("Organizations");
    },
    2 * WAIT.timeout + 2 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test(
    "Create organization ends on the heading of the new organization's page",
    async () => {
      renderConsole("/");
      await press(
        await screen.findByRole("button", { name: "Create organization" }),
      );
      const dialog = await screen.findByRole("dialog", {
        name: "Create an organization",
      });
      await act(async () => {
        fireEvent.input(
          within(dialog).getByRole("textbox", { name: "Organization name" }),
          { target: { value: "Initech" } },
        );
      });
      await act(async () => {
        fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
      });
      await waitFor(
        () => expect(window.location.pathname).toMatch(/^\/orgs\/.+/),
        WAIT,
      );
      await expectHeadingFocused("Sandboxes");
    },
    2 * WAIT.timeout + 2 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );
});

describe("focus stays where it belongs", () => {
  test(
    "Escape and Cancel return the focus to the opener, not to the heading",
    async () => {
      renderConsole(`/orgs/${MOCK_IDS.acme}/members`);
      const row = (await screen.findByText("user-member-ada")).closest("li")!;
      const opener = within(row).getByRole("button", { name: /^Remove/ });

      await press(opener);
      await screen.findByRole("alertdialog");
      await act(async () => {
        fireEvent.keyDown(document.activeElement as Element, { key: "Escape" });
      });
      await waitFor(
        () => expect(screen.queryByRole("alertdialog")).toBeNull(),
        WAIT,
      );
      await settle(60);
      expect(document.activeElement).toBe(opener);

      await press(opener);
      const dialog = await screen.findByRole("alertdialog");
      await act(async () => {
        fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
      });
      await waitFor(
        () => expect(screen.queryByRole("alertdialog")).toBeNull(),
        WAIT,
      );
      await settle(60);
      expect(document.activeElement).toBe(opener);
    },
    2 * WAIT.timeout + 3 * DEFAULT_WAIT_MS + TEST_SETUP_MS,
  );

  test("the first render leaves the focus on the page body", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`);
    await screen.findByText("user-member-ada");
    await settle(60);
    expect(document.activeElement).toBe(document.body);
  });

  test("every page heading is a focus target and not a Tab stop", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/members`);
    const heading = await screen.findByRole("heading", {
      level: 1,
      name: "Members",
    });
    expect(heading.getAttribute("tabindex")).toBe("-1");
    expect(heading.tabIndex).toBe(-1);
  });
});

describe("focusPageHeading", () => {
  function page() {
    document.body.innerHTML =
      '<main><h1 tabindex="-1">Title</h1><input aria-label="Field" /><button>Go</button></main>';
    return {
      heading: document.querySelector("h1")!,
      field: document.querySelector("input")!,
      button: document.querySelector("button")!,
    };
  }

  test("focuses the heading when nothing holds the focus", () => {
    const { heading } = page();
    focusPageHeading();
    expect(document.activeElement).toBe(heading);
  });

  test("leaves a focused field or button alone", () => {
    const { field, button } = page();
    field.focus();
    focusPageHeading();
    expect(document.activeElement).toBe(field);
    button.focus();
    focusPageHeadingAfterNavigation();
    expect(document.activeElement).toBe(button);
  });

  test("does nothing without a heading", () => {
    document.body.innerHTML = "<main><button>Go</button></main>";
    focusPageHeading();
    expect(document.activeElement).toBe(document.body);
  });
});
