// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "./tooltip.tsx";

afterEach(() => {
  cleanup();
});

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
});

describe("TextField", () => {
  test("connects the label and description to the input", () => {
    render(<TextField label="Email" description="We never share it." />);

    const input = screen.getByRole("textbox", { name: "Email" });
    const describedBy = input.getAttribute("aria-describedby") ?? "";
    const description = screen.getByText("We never share it.");
    expect(describedBy.split(" ")).toContain(description.id);
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
    const css = readFileSync(new URL("./styles.css", import.meta.url), "utf8");
    const block = (selector: string): string => {
      const escaped = selector.replace(/[.[\]="]/g, "\\$&");
      const match = new RegExp(`${escaped}\\s*\\{([^}]*)\\}`).exec(css);
      expect(match).not.toBeNull();
      // Collapse formatter line wraps so declarations compare as one line.
      return (match?.[1] ?? "")
        .replace(/\s+/g, " ")
        .replace(/\( /g, "(")
        .replace(/ \)/g, ")")
        .trim();
    };

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
