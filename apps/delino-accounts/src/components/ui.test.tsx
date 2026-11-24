// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { afterEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { useState } from "react";
import {
  ConfirmDialog,
  FormDialog,
  type FormSubmission,
  formInvalid,
  formSubmitted,
} from "./ui.tsx";

afterEach(cleanup);

/** A failure whose message the dialog shows. */
function failure(message: string) {
  return new ConnectError(message, Code.AlreadyExists);
}

/** A promise that the test settles by hand. */
function deferred() {
  let resolve!: () => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<unknown>((res, rej) => {
    resolve = () => res(undefined);
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Lets the settled promise and the state updates behind it run. */
async function settle() {
  await act(async () => {
    await Promise.resolve();
  });
}

function ConfirmHarness({ onConfirm }: { onConfirm: () => Promise<unknown> }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      <ConfirmDialog
        open={open}
        title="Delete it"
        description="This cannot be undone."
        confirmLabel="Delete"
        onConfirm={onConfirm}
        onOpenChange={setOpen}
      />
    </>
  );
}

function FormHarness({
  onSubmit,
}: {
  onSubmit: () => Promise<FormSubmission | unknown>;
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      <FormDialog
        open={open}
        title="Create it"
        submitLabel="Create"
        // A resolved test promise stands for a successful request; a test that
        // needs another outcome resolves with a `FormSubmission` itself.
        onSubmit={async () => {
          const value = await onSubmit();
          return isFormSubmission(value) ? value : formSubmitted();
        }}
        onOpenChange={setOpen}
      >
        <p>Fields</p>
      </FormDialog>
    </>
  );
}

function isFormSubmission(value: unknown): value is FormSubmission {
  return typeof value === "object" && value !== null && "outcome" in value;
}

function openDialog() {
  fireEvent.click(screen.getByRole("button", { name: "Open" }));
}

function closeDialog() {
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
}

describe("ConfirmDialog", () => {
  test("calls onConfirm once for two clicks before it settles", async () => {
    const pending = deferred();
    let calls = 0;
    render(
      <ConfirmHarness
        onConfirm={() => {
          calls += 1;
          return pending.promise;
        }}
      />,
    );
    openDialog();
    const confirm = screen.getByRole("button", { name: "Delete" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    expect(calls).toBe(1);
    expect(
      (screen.getByRole("button", { name: "Delete" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    pending.resolve();
    await settle();
    // A resolved onConfirm closes the dialog.
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
    expect(calls).toBe(1);
  });

  test("a rejection shows the error and enables a retry", async () => {
    const first = deferred();
    const second = deferred();
    const attempts = [first, second];
    let calls = 0;
    render(<ConfirmHarness onConfirm={() => attempts[calls++].promise} />);
    openDialog();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    first.reject(failure("backend refused"));
    await settle();
    expect(screen.getByText(/backend refused/)).toBeTruthy();
    const confirm = screen.getByRole("button", {
      name: "Delete",
    }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);
    expect(calls).toBe(2);
    // The retry removes the notice of the failure.
    expect(screen.queryByText(/backend refused/)).toBeNull();
    second.resolve();
    await settle();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  test("a reopened dialog starts without the pending state", async () => {
    const first = deferred();
    const second = deferred();
    const attempts = [first, second];
    let calls = 0;
    render(<ConfirmHarness onConfirm={() => attempts[calls++].promise} />);
    openDialog();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    closeDialog();
    openDialog();
    const confirm = screen.getByRole("button", {
      name: "Delete",
    }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);
    expect(calls).toBe(2);
    // The late failure of the first opening changes nothing.
    first.reject(failure("old failure"));
    await settle();
    expect(screen.queryByText(/old failure/)).toBeNull();
    expect(
      (screen.getByRole("button", { name: "Delete" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    second.resolve();
    await settle();
  });
});

describe("FormDialog", () => {
  function submitButton() {
    return screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;
  }

  test("disables the submit button and blocks a second submit while pending", async () => {
    const pending = deferred();
    let calls = 0;
    render(
      <FormHarness
        onSubmit={() => {
          calls += 1;
          return pending.promise;
        }}
      />,
    );
    openDialog();
    fireEvent.click(submitButton());
    fireEvent.click(submitButton());
    expect(calls).toBe(1);
    expect(submitButton().disabled).toBe(true);
    pending.resolve();
    await settle();
    expect(submitButton().disabled).toBe(false);
  });

  test("shows a failure of the current opening and a retry clears it", async () => {
    const first = deferred();
    const second = deferred();
    const attempts = [first, second];
    let calls = 0;
    render(<FormHarness onSubmit={() => attempts[calls++].promise} />);
    openDialog();
    fireEvent.click(submitButton());
    first.reject(failure("backend refused"));
    await settle();
    expect(screen.getByText(/backend refused/)).toBeTruthy();
    expect(submitButton().disabled).toBe(false);
    fireEvent.click(submitButton());
    expect(screen.queryByText(/backend refused/)).toBeNull();
    second.resolve();
    await settle();
    expect(screen.queryByText(/backend refused/)).toBeNull();
  });

  test("a failure that arrives while the dialog is closed never shows on reopen", async () => {
    const pending = deferred();
    render(<FormHarness onSubmit={() => pending.promise} />);
    openDialog();
    fireEvent.click(submitButton());
    closeDialog();
    pending.reject(failure("old failure"));
    await settle();
    openDialog();
    expect(screen.queryByText(/old failure/)).toBeNull();
    expect(submitButton().disabled).toBe(false);
  });

  test("reopening before the old request settles enables the button and ignores its late failure", async () => {
    const first = deferred();
    const second = deferred();
    const attempts = [first, second];
    let calls = 0;
    render(<FormHarness onSubmit={() => attempts[calls++].promise} />);
    openDialog();
    fireEvent.click(submitButton());
    closeDialog();
    openDialog();
    expect(submitButton().disabled).toBe(false);
    fireEvent.click(submitButton());
    expect(calls).toBe(2);
    first.reject(failure("old failure"));
    await settle();
    expect(screen.queryByText(/old failure/)).toBeNull();
    // The old request settling does not release the current submission.
    expect(submitButton().disabled).toBe(true);
    second.reject(failure("current failure"));
    await settle();
    expect(screen.getByText(/current failure/)).toBeTruthy();
    expect(submitButton().disabled).toBe(false);
  });
});

describe("FormDialog success effects", () => {
  function submitButton() {
    return screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;
  }

  /** A request that the test settles with a submission by hand. */
  function deferredSubmission() {
    let resolve!: (submission: FormSubmission) => void;
    let reject!: (reason: unknown) => void;
    const promise = new Promise<FormSubmission>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }

  test("runs the effects of a success of the current opening once", async () => {
    const pending = deferredSubmission();
    const effects: string[] = [];
    render(<FormHarness onSubmit={() => pending.promise} />);
    openDialog();
    fireEvent.click(submitButton());
    pending.resolve(formSubmitted(() => effects.push("done")));
    await settle();
    expect(effects).toEqual(["done"]);
  });

  test("skips the effects of a success after the form unmounts", async () => {
    const pending = deferredSubmission();
    const effects: string[] = [];
    const { unmount } = render(
      <FormHarness onSubmit={() => pending.promise} />,
    );
    openDialog();
    fireEvent.click(submitButton());
    unmount();
    pending.resolve(formSubmitted(() => effects.push("done")));
    await settle();
    expect(effects).toEqual([]);
  });

  test("skips the effects of a success that arrives while the dialog is closed", async () => {
    const pending = deferredSubmission();
    const effects: string[] = [];
    render(<FormHarness onSubmit={() => pending.promise} />);
    openDialog();
    fireEvent.click(submitButton());
    closeDialog();
    pending.resolve(formSubmitted(() => effects.push("done")));
    await settle();
    expect(effects).toEqual([]);
  });

  test("skips the effects of an earlier success after a reopen and keeps the current submission pending", async () => {
    const first = deferredSubmission();
    const second = deferredSubmission();
    const attempts = [first, second];
    let calls = 0;
    const effects: string[] = [];
    render(<FormHarness onSubmit={() => attempts[calls++]!.promise} />);
    openDialog();
    fireEvent.click(submitButton());
    closeDialog();
    openDialog();
    fireEvent.click(submitButton());
    expect(calls).toBe(2);
    first.resolve(formSubmitted(() => effects.push("first")));
    await settle();
    expect(effects).toEqual([]);
    // The dialog is still open and its own submission is still pending.
    expect(submitButton().disabled).toBe(true);
    second.resolve(formSubmitted(() => effects.push("second")));
    await settle();
    expect(effects).toEqual(["second"]);
  });

  test("runs no effects for invalid input or a failure and keeps the dialog open", async () => {
    const first = deferredSubmission();
    const second = deferredSubmission();
    const attempts = [first, second];
    let calls = 0;
    const effects: string[] = [];
    render(<FormHarness onSubmit={() => attempts[calls++]!.promise} />);
    openDialog();
    fireEvent.click(submitButton());
    first.resolve(formInvalid());
    await settle();
    expect(effects).toEqual([]);
    expect(submitButton().disabled).toBe(false);
    fireEvent.click(submitButton());
    second.reject(failure("backend refused"));
    await settle();
    expect(effects).toEqual([]);
    expect(screen.getByText(/backend refused/)).toBeTruthy();
    expect(submitButton().disabled).toBe(false);
  });
});
