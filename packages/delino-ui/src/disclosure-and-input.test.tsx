// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { readFileSync } from "node:fs";
import {
  Accordion,
  AccordionHeadingLevel,
  AccordionItem,
} from "./accordion.tsx";
import { Badge, BadgeVariant } from "./badge.tsx";
import { Card, CardDescription, CardTitle } from "./card.tsx";
import {
  Collapsible,
  CollapsiblePanel,
  CollapsibleTrigger,
} from "./collapsible.tsx";
import {
  Drawer,
  DrawerActions,
  DrawerClose,
  DrawerContent,
  DrawerDescription,
  DrawerTitle,
  DrawerTrigger,
} from "./drawer.tsx";
import {
  Menu,
  MenuContent,
  MenuItem,
  MenuSeparator,
  MenuTrigger,
} from "./menu.tsx";
import { Meter } from "./meter.tsx";
import { NumberInput } from "./number-input.tsx";
import { Pagination } from "./pagination.tsx";
import { PasswordInput } from "./password-input.tsx";
import {
  Popover,
  PopoverClose,
  PopoverContent,
  PopoverDescription,
  PopoverTitle,
  PopoverTrigger,
} from "./popover.tsx";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "./segmented-control.tsx";
import { Skeleton } from "./skeleton.tsx";
import { Slider } from "./slider.tsx";
import { block } from "./stylesheet-source.ts";
import { Toggle, ToggleGroup } from "./toggle.tsx";

afterEach(() => {
  cleanup();
});

describe("Accordion", () => {
  function Example() {
    return (
      <Accordion defaultValue={["shipping"]}>
        <AccordionItem value="shipping" title="Shipping">
          Orders arrive in five days.
        </AccordionItem>
        <AccordionItem
          value="returns"
          title="Returns"
          headingLevel={AccordionHeadingLevel.H2}
        >
          Return items within 30 days.
        </AccordionItem>
      </Accordion>
    );
  }

  test("puts each trigger in a heading of the chosen level", () => {
    render(<Example />);

    expect(
      screen.getByRole("heading", { level: 3, name: "Shipping" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("heading", { level: 2, name: "Returns" }),
    ).toBeTruthy();
  });

  test("opens one item at a time", async () => {
    render(<Example />);

    const shipping = screen.getByRole("button", { name: "Shipping" });
    const returns = screen.getByRole("button", { name: "Returns" });
    expect(shipping.getAttribute("aria-expanded")).toBe("true");
    expect(returns.getAttribute("aria-expanded")).toBe("false");

    await act(async () => {
      fireEvent.click(returns);
    });
    expect(returns.getAttribute("aria-expanded")).toBe("true");
    expect(shipping.getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByText("Return items within 30 days.")).toBeTruthy();
  });
});

describe("Collapsible", () => {
  test("shows and hides its panel from the trigger", async () => {
    render(
      <Collapsible>
        <CollapsibleTrigger>Show details</CollapsibleTrigger>
        <CollapsiblePanel>Two seats left.</CollapsiblePanel>
      </Collapsible>,
    );

    const trigger = screen.getByRole("button", { name: "Show details" });
    expect(trigger.getAttribute("data-variant")).toBe("secondary");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");

    await act(async () => {
      fireEvent.click(trigger);
    });
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    const panel = screen.getByText("Two seats left.");
    expect(panel.className).toBe("dui-collapsible-panel");
  });
});

describe("Popover", () => {
  function Example() {
    return (
      <Popover>
        <PopoverTrigger>Share</PopoverTrigger>
        <PopoverContent>
          <PopoverTitle>Share this page</PopoverTitle>
          <PopoverDescription>
            Anyone with the link can view.
          </PopoverDescription>
          <PopoverClose>Done</PopoverClose>
        </PopoverContent>
      </Popover>
    );
  }

  test("opens a dialog named by its title", async () => {
    render(<Example />);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Share" }));
    });
    const popover = screen.getByRole("dialog", { name: "Share this page" });
    expect(popover.className).toBe("dui-popover");
  });

  // happy-dom computes no layout, so this checks the stylesheet source: a popover
  // taller than the space left in the viewport must stop at that height and
  // scroll inside, as the menu and select popups do.
  test("is bounded by the available height and scrolls inside", () => {
    const rule = block(".dui-popover");
    expect(rule).toContain("max-height: var(--available-height)");
    expect(rule).toContain("overflow-y: auto");
  });

  test("closes with the Escape key", async () => {
    render(<Example />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Share" }));
    });
    const popover = screen.getByRole("dialog");

    await act(async () => {
      fireEvent.keyDown(popover, { key: "Escape" });
    });
    expect(popover.hasAttribute("data-closed")).toBe(true);
  });

  test("closes with the Escape key even when dismissible is spread in", async () => {
    // `dismissible` is omitted from the types; a JS caller or an untyped spread
    // must still not be able to disable Escape at runtime.
    const untyped = { dismissible: false } as object;
    render(
      <Popover {...untyped}>
        <PopoverTrigger>Share</PopoverTrigger>
        <PopoverContent>
          <PopoverTitle>Share this page</PopoverTitle>
          <PopoverClose>Done</PopoverClose>
        </PopoverContent>
      </Popover>,
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Share" }));
    });
    const popover = screen.getByRole("dialog");

    await act(async () => {
      fireEvent.keyDown(popover, { key: "Escape" });
    });
    expect(popover.hasAttribute("data-closed")).toBe(true);
  });
});

describe("Menu", () => {
  test("runs the chosen command", async () => {
    const actions: string[] = [];
    render(
      <Menu onAction={(value) => actions.push(value)}>
        <MenuTrigger>Options</MenuTrigger>
        <MenuContent>
          <MenuItem value="rename">Rename</MenuItem>
          <MenuSeparator />
          <MenuItem value="delete" destructive>
            Delete
          </MenuItem>
        </MenuContent>
      </Menu>,
    );

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Options" }));
    });
    expect(screen.getByRole("menu").className).toBe("dui-menu");
    const remove = screen.getByRole("menuitem", { name: "Delete" });
    expect(remove.hasAttribute("data-destructive")).toBe(true);

    await act(async () => {
      fireEvent.click(remove);
    });
    expect(actions).toEqual(["delete"]);
  });
});

describe("Drawer", () => {
  function Example() {
    return (
      <Drawer>
        <DrawerTrigger>Filters</DrawerTrigger>
        <DrawerContent>
          <DrawerTitle>Filter results</DrawerTitle>
          <DrawerDescription>Narrow the list.</DrawerDescription>
          <DrawerActions>
            <DrawerClose>Cancel</DrawerClose>
          </DrawerActions>
        </DrawerContent>
      </Drawer>
    );
  }

  test("opens a dialog named by its title", async () => {
    render(<Example />);
    expect(screen.queryByRole("dialog")).toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Filters" }));
    });
    const drawer = screen.getByRole("dialog", { name: "Filter results" });
    expect(drawer.className).toBe("dui-drawer-content");
  });

  test("closes with the Escape key", async () => {
    render(<Example />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Filters" }));
    });
    const drawer = screen.getByRole("dialog");

    await act(async () => {
      fireEvent.keyDown(drawer, { key: "Escape" });
    });
    expect(drawer.hasAttribute("data-closed")).toBe(true);
  });

  test("closes with the Escape key even when dismissible is spread in", async () => {
    // `dismissible` is omitted from the types; a JS caller or an untyped spread
    // must still not be able to disable Escape at runtime.
    const untyped = { dismissible: false } as object;
    render(
      <Drawer {...untyped}>
        <DrawerTrigger>Filters</DrawerTrigger>
        <DrawerContent>
          <DrawerTitle>Filter results</DrawerTitle>
          <DrawerActions>
            <DrawerClose>Cancel</DrawerClose>
          </DrawerActions>
        </DrawerContent>
      </Drawer>,
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Filters" }));
    });
    const drawer = screen.getByRole("dialog");

    await act(async () => {
      fireEvent.keyDown(drawer, { key: "Escape" });
    });
    expect(drawer.hasAttribute("data-closed")).toBe(true);
  });
});

describe("Slider", () => {
  test("names a single thumb with the label", () => {
    render(<Slider label="Volume" defaultValue={40} showValue />);

    const thumb = screen.getByRole("slider", { name: "Volume" });
    expect((thumb as HTMLInputElement).value).toBe("40");
    expect(screen.getByText("40")).toBeTruthy();
  });

  test("renders one named thumb per range value", () => {
    render(
      <Slider
        label="Price"
        defaultValue={[20, 80]}
        thumbLabels={["Minimum price", "Maximum price"]}
      />,
    );

    const minimum = screen.getByRole("slider", { name: "Minimum price" });
    const maximum = screen.getByRole("slider", { name: "Maximum price" });
    expect((minimum as HTMLInputElement).value).toBe("20");
    expect((maximum as HTMLInputElement).value).toBe("80");
  });

  test("moves with the arrow keys", async () => {
    const values: (number | number[])[] = [];
    render(
      <Slider
        label="Volume"
        defaultValue={40}
        onValueChange={(value) => values.push(value)}
      />,
    );

    const thumb = screen.getByRole("slider", { name: "Volume" });
    await act(async () => {
      fireEvent.keyDown(thumb, { key: "ArrowRight" });
    });
    expect(values).toEqual([41]);
  });
});

describe("NumberInput", () => {
  test("steps the value with its buttons", async () => {
    const values: (number | null)[] = [];
    render(
      <NumberInput
        label="Tickets"
        defaultValue={1}
        min={1}
        onValueChange={(value) => values.push(value)}
      />,
    );

    const input = screen.getByRole("textbox", { name: "Tickets" });
    expect((input as HTMLInputElement).value).toBe("1");
    const increase = screen.getByRole("button", { name: "Increase" });
    const decrease = screen.getByRole("button", { name: "Decrease" });
    expect(decrease.hasAttribute("data-disabled")).toBe(true);

    await act(async () => {
      fireEvent.click(increase);
    });
    expect(values).toEqual([2]);
  });

  test("marks the field invalid and shows the error message", () => {
    render(<NumberInput label="Tickets" error="Choose at least one." />);

    expect(screen.getByText("Choose at least one.")).toBeTruthy();
    expect(
      screen
        .getByRole("textbox", { name: "Tickets" })
        .getAttribute("aria-invalid"),
    ).toBe("true");
  });
});

describe("PasswordInput", () => {
  // happy-dom computes no layout, so this checks the stylesheet source. The row
  // is a flex box that mirrors in right-to-left text, so its padding must be
  // logical: 14px on the input side and 4px on the toggle side.
  test("pads the row with logical properties", () => {
    const rule = block(".dui-password-input");
    expect(rule).toContain("padding-inline: 14px 4px");
    expect(rule).toContain("padding-block: 0");
    expect(rule).not.toMatch(/(^|[\s;])padding:/);
  });

  test("shows and hides the password from its toggle", async () => {
    const { container } = render(
      <PasswordInput label="Password" autoComplete="current-password" />,
    );

    const input = container.querySelector("input");
    expect(input?.getAttribute("type")).toBe("password");
    const toggle = screen.getByRole("button", { name: "Show password" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");

    await act(async () => {
      fireEvent.click(toggle);
    });
    expect(input?.getAttribute("type")).toBe("text");
    expect(toggle.getAttribute("aria-pressed")).toBe("true");
    expect(toggle.getAttribute("aria-label")).toBe("Show password");
  });

  test("names the input with the label", () => {
    const { container } = render(<PasswordInput label="Password" />);

    expect(screen.getByLabelText("Password")).toBe(
      container.querySelector("input"),
    );
  });
});

describe("SegmentedControl", () => {
  test("checks one item at a time", async () => {
    render(
      <SegmentedControl aria-label="View" defaultValue="list">
        <SegmentedControlItem value="list">List</SegmentedControlItem>
        <SegmentedControlItem value="grid">Grid</SegmentedControlItem>
      </SegmentedControl>,
    );

    const list = screen.getByRole("radio", { name: "List" });
    const grid = screen.getByRole("radio", { name: "Grid" });
    expect(list.getAttribute("aria-checked")).toBe("true");

    await act(async () => {
      fireEvent.click(grid);
    });
    expect(grid.getAttribute("aria-checked")).toBe("true");
    expect(list.getAttribute("aria-checked")).toBe("false");
  });
});

describe("Toggle", () => {
  test("reports its pressed state", async () => {
    render(<Toggle aria-label="Bold">B</Toggle>);

    const toggle = screen.getByRole("button", { name: "Bold" });
    expect(toggle.className).toBe("dui-toggle");
    expect(toggle.getAttribute("aria-pressed")).toBe("false");

    await act(async () => {
      fireEvent.click(toggle);
    });
    expect(toggle.getAttribute("aria-pressed")).toBe("true");
  });

  test("presses one toggle of a group at a time", async () => {
    const values: string[][] = [];
    render(
      <ToggleGroup
        aria-label="Alignment"
        onValueChange={(value) => values.push(value)}
      >
        <Toggle value="left">Left</Toggle>
        <Toggle value="center">Center</Toggle>
      </ToggleGroup>,
    );

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Left" }));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Center" }));
    });
    expect(values).toEqual([["left"], ["center"]]);
  });

  // happy-dom does not compute the cascade from the stylesheet, so this
  // checks the selectors textually. The hover rule is guarded with `:not()`,
  // which raises its specificity above the pressed rule unless it also
  // excludes the pressed state.
  test("keeps the pressed surface while hovered", () => {
    const css = readFileSync(new URL("./styles.css", import.meta.url), "utf8");
    const hoverRule = /\.dui-toggle:hover[^{]*\{/.exec(css)?.[0] ?? "";
    expect(hoverRule).toContain(":not([data-disabled])");
    expect(hoverRule).toContain(":not([data-pressed])");
    expect(css).toMatch(
      /\.dui-toggle\[data-pressed\]\s*\{[^}]*background: var\(--dui-color-neutral-hover\)/,
    );
  });
});

describe("Pagination", () => {
  test("is a named navigation with the current page marked", () => {
    render(<Pagination aria-label="Pages" pageCount={3} defaultPage={2} />);

    const navigation = screen.getByRole("navigation", { name: "Pages" });
    expect(navigation.className).toBe("dui-pagination");
    expect(
      screen.getByRole("button", { name: "2" }).getAttribute("aria-current"),
    ).toBe("page");
  });

  test("reports the chosen page", async () => {
    const pages: number[] = [];
    render(
      <Pagination
        aria-label="Pages"
        pageCount={3}
        onPageChange={(page) => pages.push(page)}
      />,
    );

    const previous = screen.getByRole("button", { name: "Previous page" });
    expect(previous.getAttribute("aria-disabled")).toBe("true");

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "3" }));
    });
    expect(pages).toEqual([2, 3]);
  });
});

describe("Meter", () => {
  test("is a named meter with its value", () => {
    render(<Meter label="Storage used" value={40} showValue />);

    const meter = screen.getByRole("meter", { name: "Storage used" });
    expect(meter.getAttribute("aria-valuenow")).toBe("40");
    expect(meter.className).toBe("dui-progress");
  });
});

describe("Badge", () => {
  test("renders the variant", () => {
    render(<Badge variant={BadgeVariant.Danger}>Overdue</Badge>);

    const badge = screen.getByText("Overdue");
    expect(badge.className).toBe("dui-badge");
    expect(badge.getAttribute("data-variant")).toBe("danger");
  });
});

describe("Card", () => {
  test("is a region named by its title", () => {
    render(
      <Card aria-labelledby="plan-title">
        <CardTitle id="plan-title">Pro plan</CardTitle>
        <CardDescription>For growing teams.</CardDescription>
      </Card>,
    );

    const card = screen.getByRole("region", { name: "Pro plan" });
    expect(card.className).toBe("dui-card");
    expect(screen.getByRole("heading", { level: 3 }).textContent).toBe(
      "Pro plan",
    );
  });
});

describe("Skeleton", () => {
  test("is hidden from screen readers and takes its size", () => {
    const { container } = render(<Skeleton width="120px" circle />);

    const skeleton = container.firstElementChild as HTMLElement;
    expect(skeleton.getAttribute("aria-hidden")).toBe("true");
    expect(skeleton.hasAttribute("data-circle")).toBe(true);
    expect(skeleton.style.width).toBe("120px");
  });
});
