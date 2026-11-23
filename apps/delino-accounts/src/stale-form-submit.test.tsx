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
import { Code, ConnectError } from "@connectrpc/connect";
import { toast } from "delino-ui/toast";
import { ACCOUNT_ID, createFakeApi, type FakeApi } from "./testing/fake-api.ts";
import { deferNextCall } from "./testing/form-submit-deferral.ts";
import { renderApp, settle } from "./testing/render.tsx";

/**
 * A request of a form that succeeds after the dialog was dismissed must still
 * refresh the data, but it must not close the dialog's current opening, clear
 * its draft, navigate or show the form's toast.
 */

async function click(element: Element) {
  await act(async () => {
    fireEvent.click(element);
  });
  await settle();
}

async function type(element: Element, value: string) {
  await act(async () => {
    fireEvent.input(element, { target: { value } });
  });
}

async function pressEscape() {
  await act(async () => {
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
  });
  await settle(60);
}

function calls(api: FakeApi, method: string) {
  return api.calls.filter((c) => c.method === method).map((c) => c.request);
}

interface FormUnderTest {
  name: string;
  /** The fake API method that the form sends. */
  method: string;
  /** The method that reads the data that the success refreshes. */
  refreshMethod: string;
  firstDraft: string;
  secondDraft: string;
  invalidDraft: string;
  invalidMessage: string;
  /** Renders the page and returns the button that opens the dialog. */
  render(api: FakeApi): Promise<void>;
  openButton(): HTMLElement;
  field(): HTMLInputElement;
  submitButton(): HTMLButtonElement;
  /** The toast that a success of the draft shows. */
  toast(draft: string): RegExp;
  /** Asserts what the current success of `draft` does after the dialog. */
  expectCurrentSuccess(draft: string): Promise<void>;
  /** Asserts that nothing navigated. */
  expectNoNavigation(): void;
}

const FORMS: FormUnderTest[] = [
  {
    name: "top-up",
    method: "CreateTopUp",
    refreshMethod: "GetBalance",
    firstDraft: "30.00",
    secondDraft: "40.00",
    invalidDraft: "24.99",
    invalidMessage: "Enter at least $25.00.",
    async render(api) {
      renderApp(`/billing/${ACCOUNT_ID}`, { api });
      await screen.findByRole("heading", { name: "Acme Robotics", level: 1 });
      await settle();
    },
    openButton: () => screen.getByRole("button", { name: "Top up" }),
    field: () =>
      screen.getByRole("textbox", { name: "Amount (USD)" }) as HTMLInputElement,
    submitButton: () =>
      within(screen.getByRole("dialog")).getByRole("button", {
        name: "Top up",
      }) as HTMLButtonElement,
    toast: (draft) =>
      new RegExp(`Paddle accepted the \\$${draft.replace(".", "\\.")} top-up`),
    async expectCurrentSuccess(draft) {
      expect(shownToasts()).toEqual([expect.stringMatching(this.toast(draft))]);
    },
    expectNoNavigation() {
      expect(window.location.pathname).toBe(`/billing/${ACCOUNT_ID}`);
    },
  },
  {
    name: "invitation",
    method: "CreateInvitation",
    refreshMethod: "ListInvitations",
    firstDraft: "first@example.com",
    secondDraft: "second@example.com",
    invalidDraft: "not-an-email",
    invalidMessage: "Enter an email address like name@example.com.",
    async render(api) {
      renderApp(`/billing/${ACCOUNT_ID}`, { api });
      await screen.findByRole("heading", { name: "Acme Robotics", level: 1 });
      await click(await screen.findByRole("tab", { name: "Members" }));
      await screen.findByRole("list", { name: "Pending invitations" });
    },
    openButton: () => screen.getByRole("button", { name: "Invite by email" }),
    field: () =>
      screen.getByRole("textbox", { name: "Email" }) as HTMLInputElement,
    submitButton: () =>
      screen.getByRole("button", {
        name: "Send invitation",
      }) as HTMLButtonElement,
    toast: (draft) => new RegExp(`An invitation is sent to ${draft}`),
    async expectCurrentSuccess(draft) {
      expect(shownToasts()).toEqual([expect.stringMatching(this.toast(draft))]);
    },
    expectNoNavigation() {
      expect(window.location.pathname).toBe(`/billing/${ACCOUNT_ID}`);
    },
  },
  {
    name: "billing account creation",
    method: "CreateBillingAccount",
    refreshMethod: "ListBillingAccounts",
    firstDraft: "Northwind",
    secondDraft: "Contoso",
    invalidDraft: "   ",
    invalidMessage: "Enter a name.",
    async render(api) {
      renderApp("/billing", { api });
      await screen.findByRole("link", { name: /Acme Robotics/ });
    },
    openButton: () =>
      screen.getByRole("button", { name: "Create billing account" }),
    field: () =>
      screen.getByRole("textbox", { name: "Name" }) as HTMLInputElement,
    submitButton: () =>
      within(screen.getByRole("dialog")).getByRole("button", {
        name: "Create",
      }) as HTMLButtonElement,
    toast: () => /^$/,
    async expectCurrentSuccess(draft) {
      expect(
        await screen.findByRole("heading", { name: draft, level: 1 }),
      ).toBeTruthy();
    },
    expectNoNavigation() {
      expect(window.location.pathname).toBe("/billing");
    },
  },
];

/**
 * The toast store is global and survives a test, so a test reads the calls of
 * `toast.success` instead of the toast text in the page.
 */
let successToasts: ReturnType<typeof rs.spyOn>;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  successToasts = rs.spyOn(toast, "success");
});

afterEach(() => {
  cleanup();
  successToasts.mockRestore();
});

function shownToasts(): string[] {
  return successToasts.mock.calls.map((call) => String(call[0]));
}

describe.each(FORMS)("$name dialog", (form) => {
  async function openDialog() {
    await click(form.openButton());
    await settle(60);
    return form.field();
  }

  async function setUp() {
    const api = createFakeApi();
    await form.render(api);
    return api;
  }

  /** The text of a stale toast: only forms with a toast have one. */
  function expectNoToast() {
    if (form.name === "billing account creation") return;
    expect(shownToasts()).toEqual([]);
  }

  test("a success after the dialog was dismissed keeps the reopened draft and refreshes the data", async () => {
    const api = await setUp();
    const held = deferNextCall(api, form.method);
    await type(await openDialog(), form.firstDraft);
    await click(form.submitButton());
    expect(calls(api, form.method)).toHaveLength(1);
    await pressEscape();
    expect(screen.queryByRole("dialog")).toBeNull();
    const refreshesBefore = calls(api, form.refreshMethod).length;

    await type(await openDialog(), form.secondDraft);
    held.release();
    await settle(80);

    // The reopened dialog stays open with its own draft and is not pending.
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(form.field().value).toBe(form.secondDraft);
    expect(form.submitButton().disabled).toBe(false);
    expectNoToast();
    form.expectNoNavigation();
    // The earlier request still happened and refreshed the data.
    expect(calls(api, form.method)).toHaveLength(1);
    expect(calls(api, form.refreshMethod).length).toBeGreaterThan(
      refreshesBefore,
    );

    // The reopened form still submits normally.
    await click(form.submitButton());
    await settle(80);
    expect(calls(api, form.method)).toHaveLength(2);
    await form.expectCurrentSuccess(form.secondDraft);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("a success after the dialog was dismissed leaves a second pending submission pending", async () => {
    const api = await setUp();
    const first = deferNextCall(api, form.method);
    const second = deferNextCall(api, form.method);
    await type(await openDialog(), form.firstDraft);
    await click(form.submitButton());
    await pressEscape();
    await type(await openDialog(), form.secondDraft);
    await click(form.submitButton());
    expect(calls(api, form.method)).toHaveLength(2);
    expect(form.submitButton().disabled).toBe(true);

    first.release();
    await settle(80);
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(form.field().value).toBe(form.secondDraft);
    expect(form.submitButton().disabled).toBe(true);
    expectNoToast();
    form.expectNoNavigation();

    // The current submission settles the dialog as before.
    second.release();
    await settle(80);
    await form.expectCurrentSuccess(form.secondDraft);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("a success of the current opening closes the dialog and runs its effects", async () => {
    const api = await setUp();
    const held = deferNextCall(api, form.method);
    await type(await openDialog(), form.firstDraft);
    await click(form.submitButton());
    expect(form.submitButton().disabled).toBe(true);
    const refreshesBefore = calls(api, form.refreshMethod).length;
    held.release();
    await settle(80);
    await form.expectCurrentSuccess(form.firstDraft);
    await settle(60);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(calls(api, form.refreshMethod).length).toBeGreaterThan(
      refreshesBefore,
    );
  });

  test("invalid input keeps the dialog open with the inline message and sends nothing", async () => {
    const api = await setUp();
    await type(await openDialog(), form.invalidDraft);
    await click(form.submitButton());
    expect(screen.getByText(form.invalidMessage)).toBeTruthy();
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(form.submitButton().disabled).toBe(false);
    expect(calls(api, form.method)).toHaveLength(0);
  });

  test("a failure of the current opening stays visible and a retry can succeed", async () => {
    const api = await setUp();
    const held = deferNextCall(api, form.method);
    await type(await openDialog(), form.firstDraft);
    await click(form.submitButton());
    held.fail(new ConnectError("Paddle is unavailable", Code.Unavailable));
    await settle(80);
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("alert")).toBeTruthy();
    expect(form.field().value).toBe(form.firstDraft);
    expect(form.submitButton().disabled).toBe(false);
    expectNoToast();

    await click(form.submitButton());
    await settle(80);
    expect(calls(api, form.method)).toHaveLength(2);
    await form.expectCurrentSuccess(form.firstDraft);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("top-up dialog idempotency", () => {
  const form = FORMS[0]!;

  test("a retry of the current opening keeps its key and a stale success does not reuse it for the next opening", async () => {
    const api = createFakeApi();
    await form.render(api);
    const held = deferNextCall(api, form.method);
    await click(form.openButton());
    await settle(60);
    await type(form.field(), form.firstDraft);
    await click(form.submitButton());
    await pressEscape();

    await click(form.openButton());
    await settle(60);
    held.release();
    await settle(80);
    const failing = deferNextCall(api, form.method);
    await type(form.field(), form.secondDraft);
    await click(form.submitButton());
    failing.fail(new ConnectError("Paddle is unavailable", Code.Unavailable));
    await settle(80);
    await click(form.submitButton());
    await settle(80);

    const requests = calls(api, form.method);
    expect(requests).toHaveLength(3);
    expect(requests[2]!.idempotencyKey).toBe(requests[1]!.idempotencyKey);
    expect(requests[1]!.idempotencyKey).not.toBe(requests[0]!.idempotencyKey);
  });
});
