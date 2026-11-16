// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { act, cleanup, render } from "@testing-library/react";
import type * as React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { NumberInput } from "./number-input.tsx";
import { PasswordInput } from "./password-input.tsx";
import { RadioGroup, RadioGroupItem } from "./radio-group.tsx";
import { Select, SelectOption } from "./select.tsx";
import { Textarea } from "./textarea.tsx";
import { TextField } from "./text-field.tsx";

afterEach(() => {
  cleanup();
});

const ERROR_MESSAGE = "This field is required.";

type FieldCase = {
  name: string;
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

    test("renders the error message in the markup", () => {
      const markup = renderToStaticMarkup(
        fieldCase.field({ error: ERROR_MESSAGE }),
      );
      expect(markup).toContain(ERROR_MESSAGE);
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

describe("disabled field with an error", () => {
  // The UI SDK does not show the error of a disabled field (an error part
  // without `match` is inactive while the field is disabled).
  test("shows no error text and keeps no dangling reference", () => {
    const markup = renderToStaticMarkup(
      <TextField
        label="Name"
        description="Your full name."
        error={ERROR_MESSAGE}
        disabled
      />,
    );
    expect(markup).not.toContain(ERROR_MESSAGE);
    expect(danglingDescribedByIds(markup)).toEqual([]);
  });
});
