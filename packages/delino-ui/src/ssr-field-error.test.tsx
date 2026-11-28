// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { act, cleanup, render } from "@testing-library/react";
import type * as React from "react";
import { renderToStaticMarkup, renderToString } from "react-dom/server";
import { NumberInput } from "./number-input.tsx";
import { PasswordInput } from "./password-input.tsx";
import { RadioGroup, RadioGroupItem } from "./radio-group.tsx";
import { Select, SelectOption } from "./select.tsx";
import { Slider } from "./slider.tsx";
import { Switch } from "./switch.tsx";
import { Textarea } from "./textarea.tsx";
import { TextField } from "./text-field.tsx";

afterEach(() => {
  cleanup();
});

const ERROR_MESSAGE = "This field is required.";

const FIELD_CONTROLS = {
  TextField: "input",
  Textarea: "textarea",
  PasswordInput: "input",
  NumberInput: "input",
  Select: 'button[role="combobox"]',
  RadioGroup: 'div[role="radiogroup"]',
};

type FieldCase = {
  name: keyof typeof FIELD_CONTROLS;
  /** Renders the field with the given extra props. */
  field: (props: {
    error?: React.ReactNode;
    disabled?: boolean;
  }) => React.ReactElement;
};

const FIELD_CASES: FieldCase[] = [
  {
    name: "TextField",
    field: (props) => (
      <TextField label="Name" description="Your full name." {...props} />
    ),
  },
  {
    name: "Textarea",
    field: (props) => (
      <Textarea label="Bio" description="A short bio." {...props} />
    ),
  },
  {
    name: "PasswordInput",
    field: (props) => (
      <PasswordInput
        label="Password"
        description="Use 12 characters."
        {...props}
      />
    ),
  },
  {
    name: "NumberInput",
    field: (props) => (
      <NumberInput label="Tickets" description="At most four." {...props} />
    ),
  },
  {
    name: "Select",
    field: (props) => (
      <Select label="Role" description="Pick one." {...props}>
        <SelectOption value="admin">Admin</SelectOption>
      </Select>
    ),
  },
  {
    name: "RadioGroup",
    field: (props) => (
      <RadioGroup label="Plan" description="Billed monthly." {...props}>
        <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
      </RadioGroup>
    ),
  },
];

/**
 * Returns the ids that `aria-describedby` attributes of the markup reference
 * but that no element of the markup defines.
 */
function danglingDescribedByIds(markup: string) {
  const defined = new Set(
    Array.from(markup.matchAll(/\sid="([^"]*)"/g), (match) => match[1]),
  );
  const referenced = Array.from(
    markup.matchAll(/\saria-describedby="([^"]*)"/g),
    (match) => match[1]?.split(/\s+/) ?? [],
  ).flat();
  return referenced.filter((id) => id !== "" && !defined.has(id));
}

describe.each(FIELD_CASES)(
  "$name field error in static markup",
  (fieldCase) => {
    test("references only ids the markup defines, with and without an error", () => {
      for (const error of [undefined, ERROR_MESSAGE]) {
        const markup = renderToStaticMarkup(fieldCase.field({ error }));
        expect(danglingDescribedByIds(markup)).toEqual([]);
      }
    });

    test("renders the error slot and references its id on the control", () => {
      const container = document.createElement("div");
      container.innerHTML = renderToStaticMarkup(
        fieldCase.field({ error: ERROR_MESSAGE }),
      );

      const error = container.querySelector('[data-slot="field-error"]');
      expect(error?.textContent).toBe(ERROR_MESSAGE);
      expect(error?.id).toBeTruthy();

      const control = container.querySelector(FIELD_CONTROLS[fieldCase.name]);
      expect(control).not.toBeNull();
      expect(control?.getAttribute("aria-describedby")?.split(/\s+/)).toContain(
        error?.id,
      );
    });

    test("shows and hides the error message on the client", async () => {
      const { container, rerender } = render(
        fieldCase.field({ error: ERROR_MESSAGE }),
      );
      await act(async () => {});
      expect(container.textContent).toContain(ERROR_MESSAGE);
      expect(
        container.querySelector("[aria-invalid='true'], [data-invalid]"),
      ).not.toBeNull();

      rerender(fieldCase.field({ error: undefined }));
      await act(async () => {});
      expect(container.textContent).not.toContain(ERROR_MESSAGE);
      expect(container.querySelector("[aria-invalid='true']")).toBeNull();
    });
  },
);

describe.each([
  ["TextField", TextField],
  ["Textarea", Textarea],
] as const)("%s error state when disabled", (_name, Component) => {
  test.each(["", ERROR_MESSAGE])(
    "keeps the description and suppresses the invalid state before and after hydration (%j)",
    (error) => {
      const field = (
        <Component
          label="Name"
          description="Help text."
          error={error}
          disabled
        />
      );
      const container = document.createElement("div");
      container.innerHTML = renderToString(field);

      function expectDisabledField() {
        const control = container.querySelector("input, textarea");
        expect(control?.hasAttribute("disabled")).toBe(true);
        expect(control?.getAttribute("aria-invalid")).not.toBe("true");
        expect(
          control?.closest(".dui-text-field")?.hasAttribute("data-invalid"),
        ).toBe(false);
        expect(container.textContent).not.toContain(ERROR_MESSAGE);
        const description = container.querySelector(
          ".dui-text-field-description",
        );
        expect(description?.textContent).toBe("Help text.");
        expect(
          control?.getAttribute("aria-describedby")?.split(/\s+/),
        ).toContain(description?.id);
        expect(danglingDescribedByIds(container.innerHTML)).toEqual([]);
      }

      expectDisabledField();
      render(field, { container, hydrate: true });
      expectDisabledField();
    },
  );

  test("restores the supplied error when reenabled", () => {
    const field = (disabled: boolean) => (
      <Component label="Name" error={ERROR_MESSAGE} disabled={disabled} />
    );
    const { container, getByRole, rerender } = render(field(false));
    const control = getByRole("textbox", { name: "Name" });
    const root = control.closest(".dui-text-field");

    for (const disabled of [false, true, false]) {
      rerender(field(disabled));
      expect(control.hasAttribute("disabled")).toBe(disabled);
      expect(control.getAttribute("aria-invalid") === "true").toBe(!disabled);
      expect(root?.hasAttribute("data-invalid")).toBe(!disabled);
      expect(container.textContent?.includes(ERROR_MESSAGE)).toBe(!disabled);
      expect(danglingDescribedByIds(container.innerHTML)).toEqual([]);
    }
  });
});

describe.each([
  ["number-input", NumberInput],
  ["password-input", PasswordInput],
] as const)("%s caller ARIA props in static markup", (slot, Component) => {
  test("places them on the input and none on the root", () => {
    const markup = renderToStaticMarkup(
      <Component
        label="Code"
        description="Help text."
        aria-describedby="a b"
        aria-label="Pass code"
      />,
    );
    const root = markup.match(new RegExp(`<div[^>]*data-slot="${slot}"[^>]*>`));
    expect(root?.[0]).toBeTruthy();
    expect(root?.[0]).not.toContain("aria-");
    const input = markup.match(/<input[^>]*>/)?.[0] ?? "";
    expect(input).toMatch(/aria-describedby="a b [^"]+"/);
    expect(input).toContain('aria-label="Pass code"');
  });
});

type DescriptionCase = {
  name: string;
  /** Selects the element that carries `aria-describedby`. */
  control: string;
  /** Renders the component with the given description. */
  field: (description: React.ReactNode) => React.ReactElement;
};

const DESCRIPTION_CASES: DescriptionCase[] = [
  {
    name: "TextField",
    control: FIELD_CONTROLS.TextField,
    field: (description) => (
      <TextField label="Name" description={description} />
    ),
  },
  {
    name: "Textarea",
    control: FIELD_CONTROLS.Textarea,
    field: (description) => <Textarea label="Bio" description={description} />,
  },
  {
    name: "PasswordInput",
    control: FIELD_CONTROLS.PasswordInput,
    field: (description) => (
      <PasswordInput label="Password" description={description} />
    ),
  },
  {
    name: "NumberInput",
    control: FIELD_CONTROLS.NumberInput,
    field: (description) => (
      <NumberInput label="Tickets" description={description} />
    ),
  },
  {
    name: "Select",
    control: FIELD_CONTROLS.Select,
    field: (description) => (
      <Select label="Role" description={description}>
        <SelectOption value="admin">Admin</SelectOption>
      </Select>
    ),
  },
  {
    name: "RadioGroup",
    control: FIELD_CONTROLS.RadioGroup,
    field: (description) => (
      <RadioGroup label="Plan" description={description}>
        <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
      </RadioGroup>
    ),
  },
  {
    name: "Slider",
    control: 'input[role="slider"]',
    field: (description) => (
      <Slider label="Volume" defaultValue={40} description={description} />
    ),
  },
  {
    name: "Switch",
    control: 'button[role="switch"]',
    field: (description) => <Switch label="Alerts" description={description} />,
  },
];

const EMPTY_DESCRIPTIONS: [string, React.ReactNode][] = [
  ["undefined", undefined],
  ["null", null],
  ["false", false],
  ["an empty string", ""],
];

describe.each(DESCRIPTION_CASES)(
  "$name description in static markup",
  (descriptionCase) => {
    test.each(EMPTY_DESCRIPTIONS)(
      "has no unresolved aria-describedby id for %s",
      (_name, description) => {
        const markup = renderToStaticMarkup(descriptionCase.field(description));
        expect(danglingDescribedByIds(markup)).toEqual([]);
      },
    );

    test.each(EMPTY_DESCRIPTIONS)(
      "renders only an empty hidden description element for %s",
      (_name, description) => {
        const markup = renderToStaticMarkup(descriptionCase.field(description));
        const elements = Array.from(
          markup.matchAll(
            /<[^>]*data-slot="field-description"[^>]*>(.*?)<\/p>/g,
          ),
        );
        expect(elements.length).toBe(1);
        expect(elements[0]?.[0]).toMatch(/\shidden=""/);
        expect(elements[0]?.[1]).toBe("");
      },
    );

    test("renders and references a present description", () => {
      const markup = renderToStaticMarkup(descriptionCase.field("Help text."));
      expect(markup).toContain("Help text.");
      const element = markup.match(
        /<[^>]*data-slot="field-description"[^>]*>/,
      )?.[0];
      expect(element).toBeDefined();
      expect(element).not.toContain("hidden");
      const descriptionId = element?.match(/\sid="([^"]+)"/)?.[1];
      expect(descriptionId).toBeTruthy();
      const container = document.createElement("div");
      container.innerHTML = markup;
      const control = container.querySelector(descriptionCase.control);
      expect(control).not.toBeNull();
      const describedBy = control?.getAttribute("aria-describedby");
      expect(describedBy?.split(/\s+/)).toContain(descriptionId);
      expect(danglingDescribedByIds(markup)).toEqual([]);
    });
  },
);
