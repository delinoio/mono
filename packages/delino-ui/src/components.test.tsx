// SPDX-License-Identifier: Apache-2.0
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
    const describedBy = input.getAttribute("aria-describedby") ?? "";
    const description = screen.getByText("We never share it.");
    expect(describedBy.split(" ")).toContain(description.id);
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
    expect((input.getAttribute("aria-describedby") ?? "").split(" ")).toContain(
      nodes[0]?.id,
    );
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
    expect(
      (control.getAttribute("aria-describedby") ?? "").split(" "),
    ).toContain(nodes[0]?.id);
  });

  // happy-dom can neither lay out nor match `:dir()`, so this checks the
  // stylesheet source: the checked thumb travels to the inline-end edge.
  test("reverses the checked thumb travel in right-to-left text", () => {
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
    expect(
      (textarea.getAttribute("aria-describedby") ?? "").split(" "),
    ).toContain(nodes[0]?.id);
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
    test(`renders a hidden empty description node for a ${name} description`, async () => {
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
    expect(
      (control.getAttribute("aria-describedby") ?? "").split(" "),
    ).toContain(nodes[0]?.id);
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
    expect((group.getAttribute("aria-describedby") ?? "").split(" ")).toContain(
      nodes[0]?.id,
    );
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
    expect(
      (trigger.getAttribute("aria-describedby") ?? "").split(" "),
    ).toContain(nodes[0]?.id);
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
      expect(base).toContain("left: 50%");
      expect(base).toContain("translate: -50% 0");
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
});

describe("Progress", () => {
  test("is a named progress bar with its value", () => {
    render(<Progress label="Uploading" value={40} showValue />);

    const bar = screen.getByRole("progressbar", { name: "Uploading" });
    expect(bar.getAttribute("aria-valuenow")).toBe("40");
    expect(screen.getByText("40%")).toBeTruthy();
  });

  test("is indeterminate without a value", () => {
    render(<Progress label="Loading" value={null} />);

    const bar = screen.getByRole("progressbar", { name: "Loading" });
    expect(bar.hasAttribute("aria-valuenow")).toBe(false);
    expect(bar.hasAttribute("data-indeterminate")).toBe(true);
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
