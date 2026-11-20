// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { useState } from "react";
import { SecretDialog } from "./components/secret-dialog.tsx";

afterEach(() => {
  cleanup();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

function Harness({
  writeClipboard,
  onClose = () => {},
}: {
  writeClipboard: (text: string) => Promise<void>;
  onClose?: () => void;
}) {
  const [secret, setSecret] = useState<string | undefined>("osk_live_secret");
  return (
    <SecretDialog
      secret={secret}
      title="API key"
      description="Use this key in your program."
      secretLabel="API key"
      writeClipboard={writeClipboard}
      onClose={() => {
        setSecret(undefined);
        onClose();
      }}
    />
  );
}

describe("SecretDialog", () => {
  test("shows the secret once with the statement that it will not appear again", async () => {
    await act(async () => {
      render(<Harness writeClipboard={() => Promise.resolve()} />);
    });
    const dialog = screen.getByRole("dialog", { name: "API key" });
    expect(dialog.textContent).toContain("This will not be shown again");
    const value = screen.getByLabelText("API key", { selector: "textarea" });
    expect(value).toBe(screen.getByDisplayValue("osk_live_secret"));
    expect(value.hasAttribute("readonly")).toBe(true);
  });

  test("copies the secret and reports it", async () => {
    const copied: string[] = [];
    await act(async () => {
      render(
        <Harness
          writeClipboard={(text) => {
            copied.push(text);
            return Promise.resolve();
          }}
        />,
      );
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    });
    expect(copied).toEqual(["osk_live_secret"]);
    expect(screen.getByRole("status").textContent).toBe(
      "Copied to the clipboard.",
    );
  });

  test("reports a failed copy", async () => {
    await act(async () => {
      render(
        <Harness writeClipboard={() => Promise.reject(new Error("denied"))} />,
      );
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    });
    expect(screen.getByRole("status").textContent).toContain("Copying failed");
  });

  test("closes with Escape and drops the secret", async () => {
    let closed = 0;
    await act(async () => {
      render(
        <Harness
          writeClipboard={() => Promise.resolve()}
          onClose={() => {
            closed += 1;
          }}
        />,
      );
    });
    const dialog = screen.getByRole("dialog");
    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    expect(closed).toBe(1);
    expect(document.body.textContent).not.toContain("osk_live_secret");
  });

  test("never writes the secret to browser storage", async () => {
    await act(async () => {
      render(<Harness writeClipboard={() => Promise.resolve()} />);
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    });
    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
  });
});
