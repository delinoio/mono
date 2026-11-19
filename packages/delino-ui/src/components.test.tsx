// SPDX-License-Identifier: Apache-2.0
import { readdirSync, readFileSync } from "node:fs";
import { afterEach, describe, expect, test } from "@rstest/core";
import type * as React from "react";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import {
  AlertDialog,
  AlertDialogActions,
  AlertDialogClose,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "./alert-dialog.tsx";
import { Avatar, AvatarSize } from "./avatar.tsx";
import { Button, ButtonSize, ButtonVariant } from "./button.tsx";
import { Checkbox, CheckboxGroup } from "./checkbox.tsx";
import {
  Dialog,
  DialogActions,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
  DialogTrigger,
} from "./dialog.tsx";
import { NumberInput } from "./number-input.tsx";
import { PasswordInput } from "./password-input.tsx";
import { Progress } from "./progress.tsx";
import { RadioGroup, RadioGroupItem } from "./radio-group.tsx";
import { Select, SelectOption } from "./select.tsx";
import { Separator } from "./separator.tsx";
import { Switch } from "./switch.tsx";
import { Tabs, TabsList, TabsPanel, TabsTab } from "./tabs.tsx";
import { TextField } from "./text-field.tsx";
import { Textarea } from "./textarea.tsx";
import { createToaster, ToastRegion } from "./toast.tsx";
import { block, css } from "./stylesheet-source.ts";
import { Tooltip, TooltipContent, TooltipTrigger } from "./tooltip.tsx";

afterEach(() => {
  cleanup();
});

// Values of the `description` prop that mean "no description", like the
// values of the `error` prop that mean "no error".
const EMPTY_DESCRIPTIONS: [string, React.ReactNode][] = [
  ["undefined", undefined],
  ["null", null],
  ["false", false],
  ["empty string", ""],
];

/**
 * Asserts that an empty description leaves one empty, hidden description
 * element and no `aria-describedby` id without a matching element.
 */
function expectHiddenEmptyDescription(
  container: HTMLElement,
  selector: string,
) {
  const nodes = container.querySelectorAll(selector);
  expect(nodes.length).toBe(1);
  expect(nodes[0]?.hasAttribute("hidden")).toBe(true);
  expect(nodes[0]?.textContent).toBe("");
  for (const element of container.querySelectorAll("[aria-describedby]")) {
    for (const id of element.getAttribute("aria-describedby")?.split(" ") ??
      []) {
      expect(container.querySelector(`[id="${id}"]`)).not.toBeNull();
    }
  }
}

/**
 * Asserts that `control` is described by `description`. Unlike comparing
 * `aria-describedby` with `description.id` directly, this fails when the
 * description is missing, has an empty id, or the control has no
 * `aria-describedby` (where both sides would otherwise normalise to "").
 */
function expectDescribedBy(
  control: Element,
  description: Element | null | undefined,
) {
  expect(description).toBeTruthy();
  expect(description?.id).not.toBe("");
  expect(control.getAttribute("aria-describedby")?.split(" ")).toContain(
    description?.id,
  );
}

describe("Button", () => {
  test("renders a native button with the variant and size", () => {
    render(
      <Button variant={ButtonVariant.Danger} size={ButtonSize.Large}>
        Delete
      </Button>,
    );

    const button = screen.getByRole("button", { name: "Delete" });
    expect(button.tagName).toBe("BUTTON");
    expect(button.getAttribute("type")).toBe("button");
    expect(button.getAttribute("data-variant")).toBe("danger");
    expect(button.getAttribute("data-size")).toBe("large");
    expect(button.className).toBe("dui-button");
  });

  test("defaults to the primary variant and medium size", () => {
    render(<Button>Save</Button>);

    const button = screen.getByRole("button", { name: "Save" });
    expect(button.getAttribute("data-variant")).toBe("primary");
    expect(button.getAttribute("data-size")).toBe("medium");
    expect(button.hasAttribute("data-full-width")).toBe(false);
  });

  test("appends a caller class name to the package class", () => {
    render(
      <Button className="extra" fullWidth>
        Save
      </Button>,
    );

    const button = screen.getByRole("button", { name: "Save" });
    expect(button.className).toBe("dui-button extra");
    expect(button.hasAttribute("data-full-width")).toBe(true);
  });

  test("stays pending until the click handler's Promise settles", async () => {
    let finish: () => void = () => {};
    let presses = 0;
    render(
      <Button
        onClick={() => {
          presses += 1;
          return new Promise<void>((resolve) => {
            finish = resolve;
          });
        }}
      >
        Save
      </Button>,
    );

    const button = screen.getByRole("button", { name: "Save" });
    fireEvent.click(button);
    fireEvent.click(button);
    expect(presses).toBe(1);
    expect(button.hasAttribute("data-pending")).toBe(true);
    expect(button.getAttribute("aria-busy")).toBe("true");

    await act(async () => {
      finish();
    });
    expect(button.hasAttribute("data-pending")).toBe(false);
  });

  // The check is textual because happy-dom does not compute the cascade or
  // apply `:hover`.
  test("skips the hover background while disabled or pending", () => {
    const hoverBackgrounds = {
      primary: "--dui-color-accent-hover",
      secondary: "--dui-color-neutral-hover",
      danger: "--dui-color-danger-hover",
    };
    for (const [variant, token] of Object.entries(hoverBackgrounds)) {
      expect(
        block(
          `.dui-button[data-variant="${variant}"]:hover:not([data-disabled]):not([data-pending])`,
        ),
      ).toBe(`background: var(${token});`);
    }
    // The dimmed look of disabled and pending buttons is untouched.
    expect(block(".dui-button:is([data-disabled], [data-pending])")).toContain(
      "opacity: 0.5",
    );
  });
});

describe("TextField", () => {
  test("connects the label and description to the input", () => {
    render(<TextField label="Email" description="We never share it." />);

    const input = screen.getByRole("textbox", { name: "Email" });
    expectDescribedBy(input, screen.getByText("We never share it."));
  });

  for (const [name, empty] of EMPTY_DESCRIPTIONS) {
    test(`renders a hidden empty description node for a ${name} description`, async () => {
      const { container } = render(
        <TextField label="Email" description={empty} />,
      );
      await act(async () => {});

      expect(screen.getByRole("textbox", { name: "Email" })).toBeTruthy();
      expectHiddenEmptyDescription(container, ".dui-text-field-description");
    });
  }

  test("renders and references a numeric zero description", async () => {
    const { container } = render(<TextField label="Count" description={0} />);
    await act(async () => {});

    const input = screen.getByRole("textbox", { name: "Count" });
    const nodes = container.querySelectorAll(".dui-text-field-description");
    expect(nodes.length).toBe(1);
    expect(nodes[0]?.textContent).toBe("0");
    expectDescribedBy(input, nodes[0]);
  });

  test("marks the field invalid and shows the error message", () => {
    render(<TextField label="Email" error="Enter a valid email." />);

    const input = screen.getByRole("textbox", { name: "Email" });
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText("Enter a valid email.")).toBeTruthy();
  });

  test("treats an empty error message as no error", () => {
    render(<TextField label="Email" error="" />);

    const input = screen.getByRole("textbox", { name: "Email" });
    expect(input.getAttribute("aria-invalid")).toBeNull();
    expect(
      input.closest("[data-slot='field']")?.hasAttribute("data-invalid"),
    ).toBe(false);
  });

  test("reports typed values", () => {
    const values: string[] = [];
    render(
      <TextField label="Name" onValueChange={(value) => values.push(value)} />,
    );

    fireEvent.input(screen.getByRole("textbox", { name: "Name" }), {
      target: { value: "Ada" },
    });
    expect(values.at(-1)).toBe("Ada");
  });
});

describe("Switch", () => {
  test("toggles when pressed and names itself with the label", () => {
    const changes: boolean[] = [];
    render(
      <Switch
        label="Email notifications"
        onCheckedChange={(checked) => changes.push(checked)}
      />,
    );

    const control = screen.getByRole("switch", { name: "Email notifications" });
    expect(control.getAttribute("aria-checked")).toBe("false");

    fireEvent.click(control);
    expect(control.getAttribute("aria-checked")).toBe("true");
    expect(changes).toEqual([true]);
  });

  for (const [name, empty] of EMPTY_DESCRIPTIONS) {
    test(`renders a hidden empty description node for a ${name} description`, async () => {
      const { container } = render(
        <Switch label="Alerts" description={empty} />,
      );
      await act(async () => {});

      expect(screen.getByRole("switch", { name: "Alerts" })).toBeTruthy();
      expectHiddenEmptyDescription(container, ".dui-switch-description");
    });
  }

  test("describes the switch with a present description", async () => {
    const { container } = render(
      <Switch label="Alerts" description="We never share it." />,
    );
    await act(async () => {});

    const control = screen.getByRole("switch", { name: "Alerts" });
    const nodes = container.querySelectorAll(".dui-switch-description");
    expect(nodes.length).toBe(1);
    expect(nodes[0]?.textContent).toBe("We never share it.");
    expectDescribedBy(control, nodes[0]);
  });

  // happy-dom can neither lay out nor match `:dir()` or the UI SDK cascade
  // layer, so this only checks the stylesheet source text. It proves that the
  // checked-thumb rules declare the `translate` travel and reset the SDK's
  // `margin-inline-start: auto`. It does not prove the rendered thumb
  // position; that needs a real-browser layout check.
  // The UI SDK stylesheet makes every Field a column. happy-dom computes no
  // cascade, so this checks that the package rule resets the direction to row.
  test("keeps the label block and switch on one row", () => {
    expect(block(".dui-switch-field")).toContain("flex-direction: row");
  });

  test("moves the checked thumb once, and reverses it in right-to-left text", () => {
    expect(block(".dui-switch-thumb[data-checked]")).toContain(
      "margin-inline-start: 0",
    );
    expect(block(".dui-switch-thumb[data-checked]")).toContain(
      "translate: 18px 0",
    );
    expect(block(".dui-switch-thumb[data-checked]:dir(rtl)")).toContain(
      "translate: -18px 0",
    );
  });
});

describe("Dialog", () => {
  function Example() {
    return (
      <Dialog>
        <DialogTrigger>Delete account</DialogTrigger>
        <DialogContent>
          <DialogTitle>Delete your account?</DialogTitle>
          <DialogDescription>This cannot be undone.</DialogDescription>
          <DialogActions>
            <DialogClose>Cancel</DialogClose>
          </DialogActions>
        </DialogContent>
      </Dialog>
    );
  }

  test("opens from its trigger and is named by its title", async () => {
    render(<Example />);
    expect(screen.queryByRole("dialog")).toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete account" }));
    });

    const dialog = screen.getByRole("dialog", { name: "Delete your account?" });
    expect(dialog.className).toBe("dui-dialog-content");
  });

  test("closes with the Escape key", async () => {
    render(<Example />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete account" }));
    });
    const dialog = screen.getByRole("dialog");

    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    expect(dialog.hasAttribute("data-closed")).toBe(true);
  });

  test("closes with the Escape key even when dismissible is spread in", async () => {
    // `dismissible` is omitted from the types; a JS caller or an untyped spread
    // must still not be able to disable Escape at runtime.
    const untyped = { dismissible: false } as object;
    render(
      <Dialog {...untyped}>
        <DialogTrigger>Delete account</DialogTrigger>
        <DialogContent>
          <DialogTitle>Delete your account?</DialogTitle>
          <DialogClose>Cancel</DialogClose>
        </DialogContent>
      </Dialog>,
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete account" }));
    });
    const dialog = screen.getByRole("dialog");

    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    expect(dialog.hasAttribute("data-closed")).toBe(true);
  });

  test.each([
    { modal: false },
    { modal: "trap-focus" },
    { presentation: "sheet" },
  ])(
    "stays a centered modal dialog when %o is spread in",
    async (untypedProps) => {
      // `modal` and `presentation` are omitted from the types; an untyped
      // spread must still get the modal dialog with its backdrop.
      const untyped = untypedProps as object;
      render(
        <Dialog {...untyped}>
          <DialogTrigger>Delete account</DialogTrigger>
          <DialogContent>
            <DialogTitle>Delete your account?</DialogTitle>
            <DialogClose>Cancel</DialogClose>
          </DialogContent>
        </Dialog>,
      );
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Delete account" }));
      });
      const dialog = screen.getByRole("dialog");

      expect(dialog.getAttribute("aria-modal")).toBe("true");
      expect(dialog.getAttribute("data-presentation")).not.toBe("sheet");
      expect(document.querySelector(".dui-dialog-backdrop")).not.toBeNull();
    },
  );

  test("does not type-check modal or presentation props", () => {
    // Enforced by `tsc` (typecheck), not at runtime: the function is never
    // called, and each directive fails the build if the prop becomes valid.
    function NeverRendered() {
      return (
        <>
          {/* @ts-expect-error modal is omitted from DialogProps */}
          <Dialog modal={false} />
          {/* @ts-expect-error presentation is omitted from DialogProps */}
          <Dialog presentation="sheet" />
        </>
      );
    }
    expect(typeof NeverRendered).toBe("function");
  });

  test("closes from a close button", async () => {
    render(<Example />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete account" }));
    });
    const dialog = screen.getByRole("dialog");

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    });
    expect(dialog.hasAttribute("data-closed")).toBe(true);
  });

  test("centers the panel by resetting the popover inset and margin", () => {
    // The content renders as a top-layer popover whose browser and UI SDK
    // styles leave `right: 0`, `bottom: 0` and `margin: auto`; without the
    // reset the panel is displaced from the viewport center.
    const panel = block(".dui-dialog-content");
    expect(panel).toContain("right: auto");
    expect(panel).toContain("bottom: auto");
    expect(panel).toContain("margin: 0");
    expect(panel).toContain("top: 50%");
    expect(panel).toContain("left: 50%");
    expect(panel).toContain("translate: -50% -50%");
  });
});

describe("Textarea", () => {
  test("connects the label and shows the error message", () => {
    render(<Textarea label="Message" error="Enter a message." />);

    const textarea = screen.getByRole("textbox", { name: "Message" });
    expect(textarea.tagName).toBe("TEXTAREA");
    expect(textarea.className).toBe("dui-textarea");
    expect(textarea.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText("Enter a message.")).toBeTruthy();
  });

  for (const [name, empty] of EMPTY_DESCRIPTIONS) {
    test(`renders a hidden empty description node for a ${name} description`, async () => {
      const { container } = render(
        <Textarea label="Message" description={empty} />,
      );
      await act(async () => {});

      expect(screen.getByRole("textbox", { name: "Message" })).toBeTruthy();
      expectHiddenEmptyDescription(container, ".dui-text-field-description");
    });
  }

  test("describes the textarea with a present description", async () => {
    const { container } = render(
      <Textarea label="Message" description="Keep it short." />,
    );
    await act(async () => {});

    const textarea = screen.getByRole("textbox", { name: "Message" });
    const nodes = container.querySelectorAll(".dui-text-field-description");
    expect(nodes.length).toBe(1);
    expect(nodes[0]?.textContent).toBe("Keep it short.");
    expectDescribedBy(textarea, nodes[0]);
  });
});

describe("autoFocus inside Dialog", () => {
  // The UI SDK dialog picks its initial focus target with a `[autofocus],
  // [data-autofocus]` query, but React does not render an `autofocus`
  // attribute for the `autoFocus` prop. The wrappers add `data-autofocus`.
  function DialogWith({ children }: { children?: React.ReactNode }) {
    return (
      <Dialog>
        <DialogTrigger>Open</DialogTrigger>
        <DialogContent>
          <DialogClose>Close</DialogClose>
          <DialogTitle>Rename</DialogTitle>
          {children}
        </DialogContent>
      </Dialog>
    );
  }

  async function openDialog() {
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Open" }));
    });
    // Wait for the open effects that move focus into the dialog.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
  }

  test("focuses a TextField with autoFocus even after the close button", async () => {
    render(
      <DialogWith>
        <TextField label="Name" autoFocus />
      </DialogWith>,
    );
    await openDialog();

    const input = screen.getByRole("textbox", { name: "Name" });
    expect(input.getAttribute("data-autofocus")).not.toBeNull();
    expect(document.activeElement).toBe(input);
  });

  test("focuses a Textarea with autoFocus even after the close button", async () => {
    render(
      <DialogWith>
        <Textarea label="Message" autoFocus />
      </DialogWith>,
    );
    await openDialog();

    const textarea = screen.getByRole("textbox", { name: "Message" });
    expect(textarea.getAttribute("data-autofocus")).not.toBeNull();
    expect(document.activeElement).toBe(textarea);
  });

  test("focuses a NumberInput with autoFocus even after the close button", async () => {
    render(
      <DialogWith>
        <NumberInput label="Amount" autoFocus />
      </DialogWith>,
    );
    await openDialog();

    const input = screen.getByRole("textbox", { name: "Amount" });
    expect(input.getAttribute("data-autofocus")).not.toBeNull();
    expect(document.activeElement).toBe(input);
  });

  test("focuses a PasswordInput with autoFocus even after the close button", async () => {
    render(
      <DialogWith>
        <PasswordInput label="Password" autoFocus />
      </DialogWith>,
    );
    await openDialog();

    // A password input has no `textbox` role, so find it by its label.
    const input = screen.getByLabelText("Password");
    expect(input.getAttribute("data-autofocus")).not.toBeNull();
    expect(document.activeElement).toBe(input);
  });

  test("focuses the first tabbable element without an autoFocus input", async () => {
    render(
      <DialogWith>
        <TextField label="Name" />
        <Textarea label="Message" />
      </DialogWith>,
    );
    await openDialog();

    expect(
      screen
        .getByRole("textbox", { name: "Name" })
        .hasAttribute("data-autofocus"),
    ).toBe(false);
    expect(
      screen
        .getByRole("textbox", { name: "Message" })
        .hasAttribute("data-autofocus"),
    ).toBe(false);
    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "Close" }),
    );
  });

  test("renders no data-autofocus on NumberInput and PasswordInput without autoFocus", async () => {
    render(
      <DialogWith>
        <NumberInput label="Amount" />
        <PasswordInput label="Password" />
      </DialogWith>,
    );
    await openDialog();

    expect(
      screen
        .getByRole("textbox", { name: "Amount" })
        .hasAttribute("data-autofocus"),
    ).toBe(false);
    expect(
      screen.getByLabelText("Password").hasAttribute("data-autofocus"),
    ).toBe(false);
    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "Close" }),
    );
  });

  // A caller `data-autofocus` is spread onto the non-tabbable root element. The
  // UI SDK dialog takes the first `[data-autofocus]` match in document order and
  // does not try later matches, so a root match must never exist.
  test("keeps the NumberInput control as the only data-autofocus target with autoFocus and a caller data-autofocus", async () => {
    render(
      <DialogWith>
        <NumberInput label="Amount" autoFocus data-autofocus="" />
      </DialogWith>,
    );
    await openDialog();

    const input = screen.getByRole("textbox", { name: "Amount" });
    const targets = document.querySelectorAll("[data-autofocus]");
    expect(targets.length).toBe(1);
    expect(targets[0]).toBe(input);
    expect(document.activeElement).toBe(input);
  });

  test("keeps the PasswordInput control as the only data-autofocus target with autoFocus and a caller data-autofocus", async () => {
    render(
      <DialogWith>
        <PasswordInput label="Password" autoFocus data-autofocus="" />
      </DialogWith>,
    );
    await openDialog();

    const input = screen.getByLabelText("Password");
    const targets = document.querySelectorAll("[data-autofocus]");
    expect(targets.length).toBe(1);
    expect(targets[0]).toBe(input);
    expect(document.activeElement).toBe(input);
  });

  test("ignores a caller data-autofocus on NumberInput without autoFocus", async () => {
    render(
      <DialogWith>
        <NumberInput label="Amount" data-autofocus="" />
      </DialogWith>,
    );
    await openDialog();

    expect(document.querySelectorAll("[data-autofocus]").length).toBe(0);
    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "Close" }),
    );
  });

  test("ignores a caller data-autofocus on PasswordInput without autoFocus", async () => {
    render(
      <DialogWith>
        <PasswordInput label="Password" data-autofocus="" />
      </DialogWith>,
    );
    await openDialog();

    expect(document.querySelectorAll("[data-autofocus]").length).toBe(0);
    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "Close" }),
    );
  });

  test("focuses a TextField with autoFocus on mount outside a dialog", () => {
    render(<TextField label="Name" autoFocus />);

    expect(document.activeElement).toBe(
      screen.getByRole("textbox", { name: "Name" }),
    );
  });

  test("focuses a Textarea with autoFocus on mount outside a dialog", () => {
    render(<Textarea label="Message" autoFocus />);

    expect(document.activeElement).toBe(
      screen.getByRole("textbox", { name: "Message" }),
    );
  });

  test("focuses a NumberInput with autoFocus on mount outside a dialog", () => {
    render(<NumberInput label="Amount" autoFocus />);

    expect(document.activeElement).toBe(
      screen.getByRole("textbox", { name: "Amount" }),
    );
  });

  test("focuses a PasswordInput with autoFocus on mount outside a dialog", () => {
    render(<PasswordInput label="Password" autoFocus />);

    expect(document.activeElement).toBe(screen.getByLabelText("Password"));
  });
});

describe("Checkbox", () => {
  test("toggles when its label is pressed", () => {
    const changes: unknown[] = [];
    render(
      <Checkbox
        label="Accept the terms"
        description="Read them first."
        onCheckedChange={(checked) => changes.push(checked)}
      />,
    );

    const control = screen.getByRole("checkbox", { name: "Accept the terms" });
    expect(control.getAttribute("aria-checked")).toBe("false");

    fireEvent.click(screen.getByText("Accept the terms"));
    expect(control.getAttribute("aria-checked")).toBe("true");
    expect(changes).toEqual([true]);
  });

  for (const [name, empty] of EMPTY_DESCRIPTIONS) {
    test(`renders no description node for a ${name} description`, async () => {
      const { container } = render(
        <Checkbox label="Accept the terms" description={empty} />,
      );
      await act(async () => {});

      const control = screen.getByRole("checkbox", {
        name: "Accept the terms",
      });
      expect(container.querySelector(".dui-checkbox-description")).toBeNull();
      expect(control.hasAttribute("aria-describedby")).toBe(false);
    });
  }

  test("describes the checkbox with a present description", async () => {
    const { container } = render(
      <Checkbox label="Accept the terms" description="Read them first." />,
    );
    await act(async () => {});

    const control = screen.getByRole("checkbox", { name: "Accept the terms" });
    const nodes = container.querySelectorAll(".dui-checkbox-description");
    expect(nodes.length).toBe(1);
    expect(nodes[0]?.textContent).toBe("Read them first.");
    expectDescribedBy(control, nodes[0]);
  });

  test("keeps the description id after a caller aria-describedby", async () => {
    render(
      <>
        <p id="extra-help">Ask an adult first.</p>
        <Checkbox
          label="Accept the terms"
          description="Read them first."
          aria-describedby="extra-help"
        />
      </>,
    );
    await act(async () => {});

    const control = screen.getByRole("checkbox", { name: "Accept the terms" });
    const description = screen.getByText("Read them first.");
    const ids = (control.getAttribute("aria-describedby") ?? "").split(" ");
    expect(ids).toEqual(["extra-help", description.id]);
    for (const id of ids) {
      expect(document.getElementById(id)).not.toBeNull();
    }
  });

  test("keeps a multi-id caller aria-describedby as written", async () => {
    render(
      <Checkbox
        label="Accept the terms"
        description="Read them first."
        aria-describedby="a b"
      />,
    );
    await act(async () => {});

    const control = screen.getByRole("checkbox", { name: "Accept the terms" });
    const description = screen.getByText("Read them first.");
    expect(control.getAttribute("aria-describedby")).toBe(
      `a b ${description.id}`,
    );
  });

  test("uses only the caller aria-describedby without a description", async () => {
    render(<Checkbox label="Accept the terms" aria-describedby="extra-help" />);
    await act(async () => {});

    expect(
      screen
        .getByRole("checkbox", { name: "Accept the terms" })
        .getAttribute("aria-describedby"),
    ).toBe("extra-help");
  });

  test("adds no empty token for an empty caller aria-describedby", async () => {
    render(
      <Checkbox
        label="Accept the terms"
        description="Read them first."
        aria-describedby=""
      />,
    );
    await act(async () => {});

    expect(
      screen
        .getByRole("checkbox", { name: "Accept the terms" })
        .getAttribute("aria-describedby"),
    ).toBe(screen.getByText("Read them first.").id);
  });

  test("renders no aria-describedby with neither a description nor a caller value", async () => {
    render(<Checkbox label="Accept the terms" aria-describedby="" />);
    await act(async () => {});

    expect(
      screen
        .getByRole("checkbox", { name: "Accept the terms" })
        .hasAttribute("aria-describedby"),
    ).toBe(false);
  });

  test("reports the checked values of a group", () => {
    const values: string[][] = [];
    render(
      <CheckboxGroup
        aria-label="Toppings"
        defaultValue={["cheese"]}
        onValueChange={(value) => values.push(value)}
      >
        <Checkbox label="Cheese" value="cheese" />
        <Checkbox label="Olives" value="olives" />
      </CheckboxGroup>,
    );

    expect(
      screen
        .getByRole("checkbox", { name: "Cheese" })
        .getAttribute("aria-checked"),
    ).toBe("true");

    fireEvent.click(screen.getByRole("checkbox", { name: "Olives" }));
    expect(values.at(-1)).toEqual(["cheese", "olives"]);
  });

  // happy-dom computes no cascade, so the cursor is checked on the stylesheet
  // source. The row is a native label, so its rule keys on the disabled state
  // of the control inside it, which the DOM checks below pin.
  test("shows a not-allowed cursor on a disabled row only", () => {
    expect(block(".dui-checkbox-item")).toContain("cursor: pointer;");
    expect(
      block(".dui-checkbox-item:has(.dui-checkbox[data-disabled])"),
    ).toContain("cursor: not-allowed;");
  });

  test("marks the control inside the row disabled for the row cursor rule", () => {
    const { container } = render(
      <CheckboxGroup aria-label="Toppings" disabled>
        <Checkbox value="cheese" label="Cheese" />
      </CheckboxGroup>,
    );
    expect(
      container.querySelector(
        ".dui-checkbox-item .dui-checkbox[data-disabled]",
      ),
    ).not.toBeNull();
  });
});

describe("RadioGroup", () => {
  test("is named by its label and checks one option", () => {
    const values: (string | null)[] = [];
    render(
      <RadioGroup
        label="Plan"
        defaultValue="monthly"
        onValueChange={(value) => values.push(value)}
      >
        <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
        <RadioGroupItem value="yearly">Yearly</RadioGroupItem>
      </RadioGroup>,
    );

    expect(screen.getByRole("radiogroup", { name: "Plan" })).toBeTruthy();
    const monthly = screen.getByRole("radio", { name: "Monthly" });
    const yearly = screen.getByRole("radio", { name: "Yearly" });
    expect(monthly.getAttribute("aria-checked")).toBe("true");

    fireEvent.click(yearly);
    expect(yearly.getAttribute("aria-checked")).toBe("true");
    expect(monthly.getAttribute("aria-checked")).toBe("false");
    expect(values).toEqual(["yearly"]);
  });

  for (const [name, empty] of EMPTY_DESCRIPTIONS) {
    test(`renders a hidden empty description node for a ${name} description`, async () => {
      const { container } = render(
        <RadioGroup label="Plan" description={empty}>
          <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
        </RadioGroup>,
      );
      await act(async () => {});

      expect(screen.getByRole("radiogroup", { name: "Plan" })).toBeTruthy();
      expectHiddenEmptyDescription(container, ".dui-radio-field-description");
    });
  }

  test("describes the group with a present description", async () => {
    const { container } = render(
      <RadioGroup label="Plan" description="Billed every period.">
        <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
      </RadioGroup>,
    );
    await act(async () => {});

    const group = screen.getByRole("radiogroup", { name: "Plan" });
    const nodes = container.querySelectorAll(".dui-radio-field-description");
    expect(nodes.length).toBe(1);
    expect(nodes[0]?.textContent).toBe("Billed every period.");
    expectDescribedBy(group, nodes[0]);
  });

  // See the Checkbox row cursor tests: the row is a native label.
  test("shows a not-allowed cursor on a disabled row only", () => {
    expect(block(".dui-radio-item")).toContain("cursor: pointer;");
    expect(block(".dui-radio-item:has(.dui-radio[data-disabled])")).toContain(
      "cursor: not-allowed;",
    );
  });

  test("marks the control inside the row disabled for the row cursor rule", () => {
    const { container } = render(
      <RadioGroup label="Plan" disabled>
        <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
      </RadioGroup>,
    );
    expect(
      container.querySelector(".dui-radio-item .dui-radio[data-disabled]"),
    ).not.toBeNull();
  });
});

describe("Select", () => {
  test("shows the placeholder and opens a list of options", async () => {
    render(
      <Select label="Role" placeholder="Choose a role">
        <SelectOption value="viewer">Viewer</SelectOption>
        <SelectOption value="editor">Editor</SelectOption>
      </Select>,
    );

    const trigger = screen.getByRole("combobox", { name: "Role" });
    expect(trigger.className).toBe("dui-select");
    expect(trigger.textContent).toContain("Choose a role");

    await act(async () => {
      fireEvent.click(trigger);
    });
    expect(screen.getByRole("option", { name: "Editor" })).toBeTruthy();
  });

  test("reports the chosen option", async () => {
    const values: (string | null)[] = [];
    render(
      <Select label="Role" onValueChange={(value) => values.push(value)}>
        <SelectOption value="viewer">Viewer</SelectOption>
        <SelectOption value="editor">Editor</SelectOption>
      </Select>,
    );

    const trigger = screen.getByRole("combobox", { name: "Role" });
    await act(async () => {
      fireEvent.click(trigger);
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("option", { name: "Editor" }));
    });
    expect(values).toEqual(["editor"]);
    expect(trigger.textContent).toContain("Editor");
  });

  test.each([
    { name: "without multiple", untyped: {} as object },
    {
      name: "even when multiple is spread in",
      // `multiple` is omitted from the types; a JS caller or an untyped spread
      // must still not turn the select into a multi-value listbox at runtime.
      untyped: { multiple: true } as object,
    },
  ])("stays a single-value picker $name", async ({ untyped }) => {
    const values: unknown[] = [];
    render(
      <Select
        label="Role"
        value="viewer"
        onValueChange={(value) => values.push(value)}
        {...untyped}
      >
        <SelectOption value="viewer">Viewer</SelectOption>
        <SelectOption value="editor">Editor</SelectOption>
      </Select>,
    );

    await act(async () => {
      fireEvent.click(screen.getByRole("combobox", { name: "Role" }));
    });
    const listbox = screen.getByRole("listbox");
    expect(listbox.getAttribute("aria-multiselectable")).toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByRole("option", { name: "Editor" }));
    });
    expect(values).toEqual(["editor"]);
    expect(
      screen.queryByRole("listbox")?.hasAttribute("data-closed") ?? true,
    ).toBe(true);
  });

  for (const [name, empty] of EMPTY_DESCRIPTIONS) {
    test(`renders a hidden empty description node for a ${name} description`, async () => {
      const { container } = render(
        <Select label="Role" description={empty}>
          <SelectOption value="viewer">Viewer</SelectOption>
        </Select>,
      );
      await act(async () => {});

      expect(screen.getByRole("combobox", { name: "Role" })).toBeTruthy();
      expectHiddenEmptyDescription(container, ".dui-text-field-description");
    });
  }

  test("describes the trigger with a present description", async () => {
    const { container } = render(
      <Select label="Role" description="Who can edit.">
        <SelectOption value="viewer">Viewer</SelectOption>
      </Select>,
    );
    await act(async () => {});

    const trigger = screen.getByRole("combobox", { name: "Role" });
    const nodes = container.querySelectorAll(".dui-text-field-description");
    expect(nodes.length).toBe(1);
    expect(nodes[0]?.textContent).toBe("Who can edit.");
    expectDescribedBy(trigger, nodes[0]);
  });

  test("shows the selected option in the trigger", () => {
    render(
      <Select label="Role" defaultValue="editor">
        <SelectOption value="viewer">Viewer</SelectOption>
        <SelectOption value="editor">Editor</SelectOption>
      </Select>,
    );

    const trigger = screen.getByRole("combobox", { name: "Role" });
    expect(trigger.textContent).toContain("Editor");
  });

  test("insets the trigger with logical padding so right-to-left keeps 14px on the value side", () => {
    const rule = block(".dui-select");
    expect(rule).toContain("padding-inline: 14px 12px");
    expect(rule).toContain("padding-block: 0");
    expect(rule).not.toMatch(/(^|[\s;])padding:/);
  });

  test("keeps a long value on one line and truncates it with an ellipsis", () => {
    const trigger = block(".dui-select");
    expect(trigger).toContain("min-width: 0");

    const value = block(".dui-select-value");
    expect(value).toContain("min-width: 0");
    expect(value).toContain("overflow: hidden");
    expect(value).toContain("text-overflow: ellipsis");
    expect(value).toContain("white-space: nowrap");
  });

  test("renders a long value as the first child of the trigger with its full text", () => {
    const longValue = "first.last+notifications@example.com";
    render(
      <Select label="Email" defaultValue={longValue}>
        <SelectOption value={longValue}>{longValue}</SelectOption>
        <SelectOption value="short">Short</SelectOption>
      </Select>,
    );

    const trigger = screen.getByRole("combobox", { name: "Email" });
    const value = trigger.firstElementChild;
    expect(value?.tagName).toBe("SPAN");
    expect(value?.classList.contains("dui-select-value")).toBe(true);
    expect(value?.textContent).toBe(longValue);
  });
});

describe("Tabs", () => {
  test("shows the panel of the selected tab", async () => {
    render(
      <Tabs defaultValue="account">
        <TabsList aria-label="Settings">
          <TabsTab value="account">Account</TabsTab>
          <TabsTab value="password">Password</TabsTab>
        </TabsList>
        <TabsPanel value="account">Manage your profile.</TabsPanel>
        <TabsPanel value="password">Choose a new password.</TabsPanel>
      </Tabs>,
    );

    const account = screen.getByRole("tab", { name: "Account" });
    expect(account.getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tabpanel").textContent).toBe(
      "Manage your profile.",
    );

    await act(async () => {
      fireEvent.click(screen.getByRole("tab", { name: "Password" }));
    });
    expect(
      screen
        .getByRole("tab", { name: "Password" })
        .getAttribute("aria-selected"),
    ).toBe("true");
    expect(screen.getByRole("tabpanel").textContent).toBe(
      "Choose a new password.",
    );
  });

  // The vertical stylesheet rules select on the SDK `data-orientation`
  // attribute, so lock the attributes they depend on.
  test("marks every part of vertical tabs with the orientation", () => {
    render(
      <Tabs defaultValue="account" orientation="vertical">
        <TabsList aria-label="Settings">
          <TabsTab value="account">Account</TabsTab>
          <TabsTab value="password">Password</TabsTab>
        </TabsList>
        <TabsPanel value="account">Manage your profile.</TabsPanel>
        <TabsPanel value="password">Choose a new password.</TabsPanel>
      </Tabs>,
    );

    const list = screen.getByRole("tablist", { name: "Settings" });
    expect(list.getAttribute("aria-orientation")).toBe("vertical");
    expect(list.getAttribute("data-orientation")).toBe("vertical");
    expect(
      screen
        .getByRole("tab", { name: "Account" })
        .getAttribute("data-orientation"),
    ).toBe("vertical");
    expect(screen.getByRole("tabpanel").getAttribute("data-orientation")).toBe(
      "vertical",
    );
  });

  // The check is textual because happy-dom does not compute the cascade or
  // apply `:hover`.
  test("skips the hover color on disabled tabs", () => {
    expect(block(".dui-tabs-tab:hover:not([data-disabled])")).toBe(
      "color: var(--dui-color-text);",
    );
    expect(block(".dui-tabs-tab[data-disabled]")).toContain("opacity: 0.5");
    expect(block(".dui-tabs-tab[data-selected]")).toContain(
      "color: var(--dui-color-text)",
    );
  });

  test("lays vertical tabs out as a column beside the panel", () => {
    expect(block('.dui-tabs[data-orientation="vertical"]')).toContain(
      "display: flex",
    );
    const list = block('.dui-tabs-list[data-orientation="vertical"]');
    expect(list).toContain("flex-direction: column");
    expect(list).toContain(
      "border-inline-end: 1px solid var(--dui-color-neutral-hover)",
    );
    expect(list).toContain("border-bottom: 0");

    const tab = block('.dui-tabs-tab[data-orientation="vertical"]');
    expect(tab).toContain("border-inline-end: 2px solid transparent");
    expect(tab).toContain("margin-inline-end: -1px");
    expect(tab).toContain("margin-bottom: 0");
    expect(tab).toContain("border-bottom: 0");

    expect(
      block('.dui-tabs-tab[data-orientation="vertical"][data-selected]'),
    ).toContain("border-inline-end-color: var(--dui-color-text)");
    expect(block('.dui-tabs-panel[data-orientation="vertical"]')).toContain(
      "padding-top: 0",
    );
  });

  test("mirrors vertical tabs with logical properties only", () => {
    const tabsRules = css.slice(
      css.indexOf("/* Tabs */"),
      css.indexOf("/* Tooltip */"),
    );
    expect(tabsRules).not.toMatch(/\[dir=/);
    expect(tabsRules).not.toMatch(/:dir\(/);
  });
});

describe("Tooltip", () => {
  test("describes its trigger with hidden content", () => {
    render(
      <Tooltip>
        <TooltipTrigger>Share</TooltipTrigger>
        <TooltipContent>Anyone with the link can view</TooltipContent>
      </Tooltip>,
    );

    const trigger = screen.getByRole("button", { name: "Share" });
    const content = document.getElementById(
      trigger.getAttribute("aria-describedby") ?? "",
    );
    expect(content?.getAttribute("role")).toBe("tooltip");
    expect(content?.className).toBe("dui-tooltip");
    expect(content?.textContent).toBe("Anyone with the link can view");
    expect(content?.hasAttribute("hidden")).toBe(true);
  });
});

describe("ToastRegion", () => {
  // Helpers that read the authored hex tokens from the stylesheet source. The
  // focus ring and error border contrast tests below share them.
  const compact = css
    .replace(/\s+/g, " ")
    .replace(/\( /g, "(")
    .replace(/ \)/g, ")");

  // Bodies of every `@media (prefers-color-scheme: dark)` block, by brace
  // matching. `block()` returns only the first match of a selector.
  function darkMediaBodies(): string[] {
    const opener = "@media (prefers-color-scheme: dark) {";
    const bodies: string[] = [];
    let from = compact.indexOf(opener);
    while (from !== -1) {
      const start = from + opener.length;
      let depth = 1;
      let end = start;
      while (depth > 0 && end < compact.length) {
        const char = compact[end];
        if (char === "{") depth += 1;
        if (char === "}") depth -= 1;
        end += 1;
      }
      bodies.push(compact.slice(start, end - 1));
      from = compact.indexOf(opener, end);
    }
    return bodies;
  }

  function darkRule(selector: string): string[] {
    const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const pattern = new RegExp(`(?:^|[ }])${escaped} \\{([^}]*)\\}`, "g");
    return darkMediaBodies().flatMap((body) =>
      [...body.matchAll(pattern)].map((match) => match[1] ?? ""),
    );
  }

  function property(declarations: string[], name: string): string | undefined {
    const pattern = new RegExp(`(?:^|[ ;])${name}: (#[0-9a-fA-F]{6})`);
    for (const text of [...declarations].reverse()) {
      const value = pattern.exec(text)?.[1];
      if (value) return value;
    }
    return undefined;
  }

  function luminance(hex: string): number {
    const channels = [1, 3, 5].map((index) => {
      const value = Number.parseInt(hex.slice(index, index + 2), 16) / 255;
      return value <= 0.03928
        ? value / 12.92
        : ((value + 0.055) / 1.055) ** 2.4;
    });
    return (
      0.2126 * channels[0]! + 0.7152 * channels[1]! + 0.0722 * channels[2]!
    );
  }

  function contrast(first: string, second: string): number {
    const [high, low] = [luminance(first), luminance(second)].sort(
      (a, b) => b - a,
    );
    return (high! + 0.05) / (low! + 0.05);
  }

  test("shows a toast with its title, description and action", async () => {
    const toaster = createToaster();
    render(<ToastRegion toaster={toaster} aria-label="Notifications" />);

    await act(async () => {
      toaster("Event created", {
        description: "Friday at 10:00 AM",
        action: { label: "Undo" },
      });
    });

    expect(screen.getByText("Event created").className).toBe("dui-toast-title");
    expect(screen.getByText("Friday at 10:00 AM")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Undo" })).toBeTruthy();
  });

  // happy-dom computes no cascade, so this checks the stylesheet source: the
  // SDK sizes `toast-close` as a 28px icon button with a `::before` icon, and
  // the package must reset both for the text cancel label.
  test("sizes the cancel button to its label and hides the SDK icon", () => {
    expect(block(".dui-toast-button")).toContain("width: auto");
    expect(block(".dui-toast-button::before")).toContain("display: none");
  });

  // happy-dom computes no cascade, so this checks the stylesheet source. The
  // UI SDK draws its toast border only in its optional stylesheet, which no
  // app loads, so the package resets the border for every type except `error`
  // and draws the error border itself.
  describe("error border", () => {
    test("draws a 1px package-owned border on the error toast", () => {
      expect(block('.dui-toast[data-type="error"]')).toContain(
        "border: 1px solid var(--dui-color-danger-inverse)",
      );
    });

    test("resets the border on every other toast type", () => {
      expect(block('.dui-toast:not([data-type="error"])')).toContain(
        "border: 0",
      );
      expect(block(".dui-toast")).not.toMatch(/(^|[\s;])border(-width)?:/);
    });

    test("keeps the error border at 3:1 on the toast surface in both schemes", () => {
      // The toast surface is the `text` token, so it inverts in dark mode and
      // the border token uses the opposite pair to the danger text token.
      const light = [block(":root")];
      const dark = darkRule(":root");
      expect(property(light, "--dui-color-danger-inverse")).toBe("#ff6b78");
      expect(property(dark, "--dui-color-danger-inverse")).toBe("#d22030");
      expect(property(light, "--dui-color-text")).toBe("#191f28");
      expect(property(dark, "--dui-color-text")).toBe("#f9fafb");
      for (const scheme of [light, dark]) {
        const border = property(scheme, "--dui-color-danger-inverse");
        const surface = property(scheme, "--dui-color-text");
        expect(contrast(border!, surface!)).toBeGreaterThanOrEqual(3);
      }
    });

    test("does not import or re-export the UI SDK stylesheet", () => {
      const packageRoot = new URL("../", import.meta.url);
      const manifest = readFileSync(
        new URL("package.json", packageRoot),
        "utf8",
      );
      const exportsField = JSON.stringify(
        (JSON.parse(manifest) as { exports?: unknown }).exports ?? {},
      );
      expect(exportsField).not.toContain("@ui-sdk");
      expect(css).not.toMatch(/@import[^;]*ui-sdk/);
      const sourceFiles = readdirSync(new URL("./", import.meta.url)).filter(
        (name) => /\.(css|ts|tsx)$/.test(name) && !name.endsWith(".test.tsx"),
      );
      for (const name of sourceFiles) {
        const text = readFileSync(new URL(name, import.meta.url), "utf8");
        expect(text, name).not.toMatch(
          /(?:@import|import|export)[^;\n]*@ui-sdk\/[^"';\n]*\.css/,
        );
      }
    });
  });

  // happy-dom computes no cascade, so the focus ring contrast is checked from
  // the authored hex tokens. Dark mode inverts the toast (its surface is the
  // `text` token), so the ring that reaches the toast buttons must be measured
  // against that surface and not against the normal dark surface.
  describe("focus ring contrast", () => {
    test("keeps the toast surface and the button ring wired to the tokens", () => {
      expect(block(".dui-toast")).toContain(
        "background: var(--dui-color-text)",
      );
      expect(block(".dui-button:focus-visible")).toContain(
        "outline: 2px solid var(--dui-color-focus-ring)",
      );
    });

    test("keeps the light ring at 3:1 on the toast surface", () => {
      const light = [block(":root")];
      const ring = property(light, "--dui-color-focus-ring");
      const surface = property(light, "--dui-color-text");
      expect(ring).toBeDefined();
      expect(surface).toBeDefined();
      expect(contrast(ring!, surface!)).toBeGreaterThanOrEqual(3);
    });

    test("gives the dark toast buttons a ring at 3:1 on the toast surface", () => {
      const darkRoot = darkRule(":root");
      const surface = property(darkRoot, "--dui-color-text");
      // A `.dui-toast` override wins over the dark `:root` ring for the buttons.
      const ring =
        property(darkRule(".dui-toast"), "--dui-color-focus-ring") ??
        property(darkRoot, "--dui-color-focus-ring");
      expect(surface).toBeDefined();
      expect(ring).toBeDefined();
      expect(contrast(ring!, surface!)).toBeGreaterThanOrEqual(3);
    });

    test("keeps the dark ring token outside the toast", () => {
      expect(property(darkRule(":root"), "--dui-color-focus-ring")).toBe(
        "#4593fc",
      );
    });
  });

  // happy-dom cannot evaluate `env()` or logical insets, so this checks the
  // stylesheet source: the package rules sit in a higher cascade layer than the
  // UI SDK defaults and must keep the safe-area terms and logical edges.
  describe("region offsets", () => {
    test("keeps the bottom and top offsets clear of the safe area", () => {
      expect(block(".dui-toast-region")).toContain(
        "bottom: calc(16px + var(--keyboard-inset, 0px) + env(safe-area-inset-bottom, 0px))",
      );
      expect(block('.dui-toast-region[data-side="top"]')).toContain(
        "top: calc(16px + env(safe-area-inset-top, 0px))",
      );
    });

    test("follows the writing direction for start and end alignment", () => {
      const start = block('.dui-toast-region[data-align="start"]');
      const end = block('.dui-toast-region[data-align="end"]');
      expect(start).toContain(
        "inset-inline-start: calc(16px + env(safe-area-inset-left, 0px))",
      );
      expect(end).toContain(
        "inset-inline-end: calc(16px + env(safe-area-inset-right, 0px))",
      );
      // The physical reset must come first so the logical edge wins in RTL.
      expect(start.indexOf("left: auto")).toBeGreaterThanOrEqual(0);
      expect(start.indexOf("left: auto")).toBeLessThan(
        start.indexOf("inset-inline-start"),
      );
      expect(end.indexOf("left: auto")).toBeGreaterThanOrEqual(0);
      expect(end.indexOf("left: auto")).toBeLessThan(
        end.indexOf("inset-inline-end"),
      );
      expect(start).not.toMatch(/\bleft:\s*16px/);
      expect(end).not.toMatch(/\bright:\s*16px/);
    });

    test("clears the safe-area inset of the anchored edge in right-to-left text", () => {
      expect(block('.dui-toast-region[data-align="start"]:dir(rtl)')).toContain(
        "inset-inline-start: calc(16px + env(safe-area-inset-right, 0px))",
      );
      expect(block('.dui-toast-region[data-align="end"]:dir(rtl)')).toContain(
        "inset-inline-end: calc(16px + env(safe-area-inset-left, 0px))",
      );
      // The left-to-right rules keep their own-side insets.
      expect(block('.dui-toast-region[data-align="start"]')).toContain(
        "env(safe-area-inset-left, 0px)",
      );
      expect(block('.dui-toast-region[data-align="end"]')).toContain(
        "env(safe-area-inset-right, 0px)",
      );
    });

    test("keeps the centered default", () => {
      const base = block(".dui-toast-region");
      // The centered region sits in the middle of the safe area, not the
      // viewport, so both its center and its width account for the insets.
      expect(base).toContain(
        "left: calc(50% + (env(safe-area-inset-left, 0px) - env(safe-area-inset-right, 0px)) / 2)",
      );
      expect(base).toContain(
        "width: min(400px, calc(100vw - 32px - env(safe-area-inset-left, 0px) - env(safe-area-inset-right, 0px)))",
      );
      expect(base).toContain("translate: -50% 0");
      // Start and end alignments inherit the narrower width and set only
      // their own edge.
      expect(block('.dui-toast-region[data-align="start"]')).not.toContain(
        "width",
      );
      expect(block('.dui-toast-region[data-align="end"]')).not.toContain(
        "width",
      );
    });
  });
});

describe("AlertDialog", () => {
  function Example({ onAnswer }: { onAnswer: (value: unknown) => void }) {
    return (
      <AlertDialog
        onOpenChange={(open, details) => {
          if (!open) onAnswer(details.returnValue);
        }}
      >
        <AlertDialogTrigger variant={ButtonVariant.Danger}>
          Delete project
        </AlertDialogTrigger>
        <AlertDialogContent>
          <AlertDialogTitle>Delete this project?</AlertDialogTitle>
          <AlertDialogDescription>It cannot be undone.</AlertDialogDescription>
          <AlertDialogActions>
            <AlertDialogClose>Cancel</AlertDialogClose>
            <AlertDialogClose value="confirm" variant={ButtonVariant.Danger}>
              Delete
            </AlertDialogClose>
          </AlertDialogActions>
        </AlertDialogContent>
      </AlertDialog>
    );
  }

  test("opens as a named alert dialog and reports the answer", async () => {
    const answers: unknown[] = [];
    render(<Example onAnswer={(value) => answers.push(value)} />);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    });
    expect(
      screen.getByRole("alertdialog", { name: "Delete this project?" }),
    ).toBeTruthy();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    });
    expect(answers).toEqual(["confirm"]);
  });

  test("closes with the Escape key", async () => {
    render(<Example onAnswer={() => {}} />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    });
    const dialog = screen.getByRole("alertdialog");

    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    expect(dialog.hasAttribute("data-closed")).toBe(true);
  });

  test("closes with the Escape key even when dismissible is spread in", async () => {
    // See the Dialog test: the runtime must ignore `dismissible`.
    const untyped = { dismissible: false } as object;
    render(
      <AlertDialog {...untyped}>
        <AlertDialogTrigger>Delete project</AlertDialogTrigger>
        <AlertDialogContent>
          <AlertDialogTitle>Delete this project?</AlertDialogTitle>
          <AlertDialogActions>
            <AlertDialogClose>Cancel</AlertDialogClose>
          </AlertDialogActions>
        </AlertDialogContent>
      </AlertDialog>,
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    });
    const dialog = screen.getByRole("alertdialog");

    await act(async () => {
      fireEvent.keyDown(dialog, { key: "Escape" });
    });
    expect(dialog.hasAttribute("data-closed")).toBe(true);
  });
});

describe("Avatar", () => {
  test("shows a named fallback without an image", () => {
    render(<Avatar alt="Ada Lovelace" fallback="AL" size={AvatarSize.Large} />);

    const fallback = screen.getByRole("img", { name: "Ada Lovelace" });
    expect(fallback.textContent).toBe("AL");
    expect(
      fallback.closest("[data-slot='avatar']")?.getAttribute("data-size"),
    ).toBe("large");
  });

  test.each(["", "   "])("treats the blank src %j as no image", (src) => {
    const { container } = render(
      <Avatar alt="Ada Lovelace" fallback="AL" src={src} />,
    );

    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByRole("img", { name: "Ada Lovelace" }).textContent).toBe(
      "AL",
    );
  });

  test.each([{ src: null }, { src: 42 }])(
    "renders no image and does not throw for the untyped src %j",
    (untyped) => {
      // The type allows only a string, but untyped spread objects can carry
      // anything.
      const { container } = render(
        <Avatar alt="Ada Lovelace" fallback="AL" {...(untyped as object)} />,
      );

      expect(container.querySelector("img")).toBeNull();
      expect(
        screen.getByRole("img", { name: "Ada Lovelace" }).textContent,
      ).toBe("AL");
    },
  );

  test.each(["https://example.test/ada.png", " https://example.test/ada.png "])(
    "passes the src %j to the image unchanged",
    (src) => {
      const { container } = render(
        <Avatar alt="Ada Lovelace" fallback="AL" src={src} />,
      );

      expect(container.querySelector("img")?.getAttribute("src")).toBe(src);
    },
  );
});

describe("Progress", () => {
  test("is a named progress bar with its value", () => {
    render(<Progress label="Uploading" value={40} showValue />);

    const bar = screen.getByRole("progressbar", { name: "Uploading" });
    expect(bar.getAttribute("aria-valuenow")).toBe("40");
    expect(screen.getByText("40%")).toBeTruthy();
  });

  test("references its label element for its name", () => {
    const { container } = render(<Progress label="Uploading" value={40} />);

    const bar = screen.getByRole("progressbar", { name: "Uploading" });
    const labelId = bar.getAttribute("aria-labelledby");
    expect(labelId).toBeTruthy();
    expect(container.querySelector(`[id="${labelId}"]`)?.textContent).toBe(
      "Uploading",
    );
  });

  test("is indeterminate without a value", () => {
    render(<Progress label="Loading" value={null} />);

    const bar = screen.getByRole("progressbar", { name: "Loading" });
    expect(bar.hasAttribute("aria-valuenow")).toBe(false);
    expect(bar.hasAttribute("data-indeterminate")).toBe(true);
  });

  // happy-dom computes no layout, so this checks the stylesheet source. It does
  // not prove the rendered widths; that needs a real-browser check.
  test("keeps the root to one grid track so the header spans the track", () => {
    expect(block(".dui-progress")).toContain(
      "grid-template-columns: minmax(0, 1fr)",
    );
    expect(block(".dui-progress")).toContain("gap: 8px");
  });

  // happy-dom computes no animation, so this checks the stylesheet source. It
  // does not prove the rendered sweep; that needs a real-browser check.
  test("mirrors the indeterminate sweep in right-to-left text", () => {
    expect(block(".dui-progress-indicator[data-indeterminate]")).toContain(
      "animation: dui-progress-indeterminate 1.2s",
    );
    expect(
      block(".dui-progress-indicator[data-indeterminate]:dir(rtl)"),
    ).toContain("animation-name: dui-progress-indeterminate-rtl");
    // block() matches only plain rules, so read the keyframes from the
    // whitespace-collapsed stylesheet.
    const compact = css.replace(/\s+/g, " ");
    expect(compact).toMatch(
      /@keyframes dui-progress-indeterminate-rtl \{ from \{ translate: 100% 0; \} to \{ translate: -250% 0; \} \}/,
    );
  });

  test("draws state indicators with the accent graphic token", () => {
    // Progress and Meter share the Progress classes. The dark value keeps the
    // fill and the Select check at least 3:1 against their neutral surfaces,
    // which the filled-control accent (#1b64da) does not.
    for (const selector of [
      ".dui-progress-indicator",
      ".dui-slider-indicator",
    ]) {
      expect(block(selector)).toContain(
        "background: var(--dui-color-accent-graphic)",
      );
    }
    expect(block(".dui-select-option-indicator")).toContain(
      "color: var(--dui-color-accent-graphic)",
    );
    expect(css).toMatch(/:root \{[^}]*--dui-color-accent-graphic: #1b64da;/);
    expect(css).toMatch(
      /prefers-color-scheme: dark\) \{\s*:root \{[^}]*--dui-color-accent-graphic: #4593fc;/,
    );
    // Filled controls keep the original accent with its on-accent text.
    expect(css).toMatch(/--dui-color-accent: #1b64da;/);
    expect(css).not.toMatch(/--dui-color-accent: #4593fc/);
  });
});

describe("Separator", () => {
  test("renders a separator with its orientation", () => {
    render(<Separator orientation="vertical" />);

    const separator = screen.getByRole("separator");
    expect(separator.className).toBe("dui-separator");
    expect(separator.getAttribute("data-orientation")).toBe("vertical");
  });
});
