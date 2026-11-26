// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import {
  MOCK_IDS,
  MockBillingState,
  createMockTransport,
} from "./mock/mock-api.ts";
import { holdingTransport, renderConsole, settle } from "./test-support.tsx";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

/** One form dialog and the request that its submit button sends. */
interface Form {
  name: string;
  path: string;
  /** The button that opens the form. */
  opener: string | RegExp;
  /** The title of the form dialog and the name of its submit button. */
  dialog: string;
  submit: string;
  /** The RPC of the submission. */
  method: string;
  /** Makes the opened form valid; a dismissal resets its fields. */
  fill?: (dialog: HTMLElement) => Promise<void>;
  /** The title of the one-time dialog that a success opens, when there is one. */
  secret?: string;
  /** Whether a success opens another page. */
  navigates?: boolean;
  /** A read that the success refreshes. */
  refreshed?: string;
  billing?: MockBillingState;
}

const type = (label: string, value: string) => async (dialog: HTMLElement) => {
  await act(async () => {
    fireEvent.input(within(dialog).getByRole("textbox", { name: label }), {
      target: { value },
    });
  });
};

const FORMS: Form[] = [
  {
    name: "enrollment token",
    path: `/orgs/${MOCK_IDS.acme}/enrollment-tokens`,
    opener: "Create token",
    dialog: "Create an enrollment token",
    submit: "Create token",
    method: "CreateEnrollmentToken",
    secret: "Your new enrollment token",
    refreshed: "ListEnrollmentTokens",
  },
  {
    name: "API key",
    path: `/orgs/${MOCK_IDS.acme}/api-keys`,
    opener: "Create API key",
    dialog: "Create an API key",
    submit: "Create key",
    method: "CreateApiKey",
    fill: type("Name", "deploy-bot"),
    secret: "Your new API key",
    refreshed: "ListApiKeys",
  },
  {
    name: "invitation",
    path: `/orgs/${MOCK_IDS.acme}/members`,
    opener: "Invite someone",
    dialog: "Invite someone",
    submit: "Create link",
    method: "CreateInvitation",
    secret: "Your invitation link",
    refreshed: "ListInvitations",
  },
  {
    name: "template",
    path: `/orgs/${MOCK_IDS.acme}/snapshots`,
    opener: /^Make template/,
    dialog: "Make a template",
    submit: "Create template",
    method: "CreateTemplate",
    fill: type("Template name", "golden"),
    refreshed: "ListTemplates",
  },
  {
    name: "organization",
    path: "/",
    opener: "Create organization",
    dialog: "Create an organization",
    submit: "Create",
    method: "CreateOrganization",
    fill: type("Organization name", "Initech"),
    navigates: true,
    refreshed: "ListOrganizations",
  },
  {
    name: "sandbox",
    path: `/orgs/${MOCK_IDS.acme}/sandboxes`,
    opener: "Create sandbox",
    dialog: "Create a sandbox",
    submit: "Create",
    method: "CreateSandbox",
    navigates: true,
    refreshed: "ListSandboxes",
  },
  {
    name: "billing link",
    path: `/orgs/${MOCK_IDS.acme}/settings`,
    opener: "Link a billing account",
    dialog: "Link a billing account",
    submit: "Link",
    method: "LinkBillingAccount",
    fill: async (dialog) => {
      // Find outside act: a findBy wait inside act logs act warnings.
      const [first] = await within(dialog).findAllByRole("radio");
      await act(async () => {
        fireEvent.click(first!);
      });
    },
    billing: MockBillingState.BlockedUnlinked,
    refreshed: "GetBillingStatus",
  },
];

describe.each(FORMS)("$name form dialog openings", (form) => {
  async function start() {
    const mock = createMockTransport(
      form.billing ? { billing: { [MOCK_IDS.acme]: form.billing } } : undefined,
    );
    const held = holdingTransport([form.method], () => true, mock);
    renderConsole(form.path, { transport: held.transport });
    return held;
  }

  const dialogOf = () => screen.findByRole("dialog", { name: form.dialog });

  async function open() {
    const openers = await screen.findAllByRole("button", { name: form.opener });
    await act(async () => {
      fireEvent.click(openers[0]!);
    });
    await settle(60);
    const dialog = await dialogOf();
    await form.fill?.(dialog);
    await settle();
    return dialog;
  }

  const submitButton = (dialog: HTMLElement) =>
    within(dialog).getByRole("button", {
      name: form.submit,
    }) as HTMLButtonElement;

  async function submit(dialog: HTMLElement) {
    await act(async () => {
      fireEvent.click(submitButton(dialog));
    });
    await settle();
  }

  async function dismiss(dialog: HTMLElement) {
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    await settle(60);
  }

  async function settleCall(
    held: Awaited<ReturnType<typeof start>>,
    index: number,
    error?: Error,
  ) {
    await act(async () => {
      held.calls[form.method]?.[index]?.settle(error);
    });
    await settle(100);
  }

  const notice = (dialog: HTMLElement) => dialog.querySelector(".notice-error");

  const unavailable = () =>
    new ConnectError("the request failed", Code.Unavailable);

  const editableFailure = () =>
    form.method === "CreateSandbox"
      ? new ConnectError("The request is invalid", Code.InvalidArgument)
      : unavailable();

  test("a new opening starts without the pending state of a dismissed request", async () => {
    const held = await start();
    const first = await open();
    await submit(first);
    expect(submitButton(first).disabled).toBe(true);
    await dismiss(first);
    const second = await open();
    expect(submitButton(second).disabled).toBe(false);
    // The new opening can send its own request while the old one runs.
    await submit(second);
    expect(held.calls[form.method]).toHaveLength(2);
  });

  test("a late failure shows no error in the new opening and does not clear its pending state", async () => {
    const held = await start();
    const first = await open();
    await submit(first);
    await dismiss(first);
    const second = await open();
    await submit(second);
    expect(submitButton(second).disabled).toBe(true);
    await settleCall(held, 0, unavailable());
    expect(notice(second)).toBeNull();
    expect(submitButton(second).disabled).toBe(true);
    // The new request is still the only one that decides the new opening.
    await settleCall(held, 1, editableFailure());
    expect(notice(second)).not.toBeNull();
    expect(submitButton(second).disabled).toBe(false);
  });

  test("a late success does not close the new opening, open a secret dialog or move the user", async () => {
    const held = await start();
    const first = await open();
    await submit(first);
    await dismiss(first);
    const second = await open();
    const before = held.seen[form.refreshed!] ?? 0;
    await settleCall(held, 0);
    expect(screen.getByRole("dialog", { name: form.dialog })).toBe(second);
    expect(submitButton(second).disabled).toBe(false);
    expect(window.location.pathname).toBe(
      new URL(form.path, "http://x").pathname,
    );
    if (form.secret) {
      expect(screen.queryByRole("dialog", { name: form.secret })).toBeNull();
    }
    // The cache refresh of the success still ran.
    expect(held.seen[form.refreshed!] ?? 0).toBeGreaterThan(before);
  });

  test("a late success leaves the page alone when no form is open", async () => {
    const held = await start();
    const first = await open();
    await submit(first);
    await dismiss(first);
    await settleCall(held, 0);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(window.location.pathname).toBe(
      new URL(form.path, "http://x").pathname,
    );
  });

  test("the current opening sends one request, shows its failure inline and retries", async () => {
    const held = await start();
    const dialog = await open();
    await submit(dialog);
    await submit(dialog);
    expect(held.calls[form.method]).toHaveLength(1);
    await settleCall(held, 0, editableFailure());
    expect(notice(dialog)).not.toBeNull();
    expect(window.location.pathname).toBe(
      new URL(form.path, "http://x").pathname,
    );
    expect(submitButton(dialog).disabled).toBe(false);
    await submit(dialog);
    expect(notice(dialog)).toBeNull();
    expect(held.calls[form.method]).toHaveLength(2);
    await settleCall(held, 1);
    if (form.navigates) {
      expect(window.location.pathname).not.toBe(
        new URL(form.path, "http://x").pathname,
      );
    }
    expect(screen.queryByRole("dialog", { name: form.dialog })).toBeNull();
    if (form.secret) {
      expect(screen.getByRole("dialog", { name: form.secret })).toBeTruthy();
    }
  });
});
