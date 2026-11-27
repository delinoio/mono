// SPDX-License-Identifier: Apache-2.0
import { afterEach, expect, test } from "@rstest/core";
import { Field, FieldDescription, FieldLabel } from "@ui-sdk/react/field";
import { act, cleanup, render, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { renderToString } from "react-dom/server";
import { Checkbox } from "./checkbox.tsx";

afterEach(cleanup);

function serverRender(fixture: ReactElement) {
  const container = document.createElement("div");
  container.innerHTML = renderToString(fixture);
  document.body.append(container);
  return container;
}

test("names each custom checkbox before and after hydration", async () => {
  const fixture = (
    <>
      <Checkbox label="Include archived" render={<div />} />
      <Checkbox label="Show logs" render={(props) => <div {...props} />} />
      <Checkbox label="Default notifications" />
    </>
  );
  const container = serverRender(fixture);
  const scope = within(container);
  const names = ["Include archived", "Show logs", "Default notifications"];
  const controls = names.map((name) => scope.getByRole("checkbox", { name }));
  const labels = names.slice(0, 2).map((name) => scope.getByText(name));
  const references = controls
    .slice(0, 2)
    .map((control) => control.getAttribute("aria-labelledby"));

  expect(controls.map((control) => control.tagName)).toEqual([
    "DIV",
    "DIV",
    "BUTTON",
  ]);
  expect(references).toEqual(labels.map((label) => label.id));
  expect(references.every(Boolean)).toBe(true);
  expect(new Set(references).size).toBe(2);

  render(fixture, { container, hydrate: true });
  await act(async () => {});

  expect(names.map((name) => scope.getByRole("checkbox", { name }))).toEqual(
    controls,
  );
  expect(
    controls
      .slice(0, 2)
      .map((control) => control.getAttribute("aria-labelledby")),
  ).toEqual(references);
});

test.each([
  {
    expected: "Wrapper label",
    checkbox: (
      <Checkbox
        label="Visible label"
        aria-label="Wrapper label"
        render={<div />}
      />
    ),
  },
  {
    expected: "External label",
    checkbox: (
      <Checkbox
        label="Visible label"
        aria-labelledby="external"
        render={<div />}
      />
    ),
  },
  {
    expected: "Element label",
    checkbox: (
      <Checkbox
        label="Visible label"
        aria-label="Wrapper label"
        render={<div aria-label="Element label" />}
      />
    ),
  },
  {
    expected: "External label",
    checkbox: (
      <Checkbox
        label="Visible label"
        aria-label="Wrapper label"
        render={<div aria-labelledby="external" />}
      />
    ),
  },
  {
    expected: "Callback label",
    checkbox: (
      <Checkbox
        label="Visible label"
        render={(props) => <div {...props} aria-label="Callback label" />}
      />
    ),
  },
  {
    expected: "External label",
    checkbox: (
      <Checkbox
        label="Visible label"
        render={(props) => <div {...props} aria-labelledby="external" />}
      />
    ),
  },
])(
  "keeps the explicit name $expected through hydration",
  async ({ expected, checkbox }) => {
    const fixture = (
      <>
        <span id="external">External label</span>
        {checkbox}
      </>
    );
    const container = serverRender(fixture);
    const scope = within(container);
    const control = scope.getByRole("checkbox", { name: expected });
    expect(control.tagName).toBe("DIV");

    render(fixture, { container, hydrate: true });
    await act(async () => {});

    expect(scope.getByRole("checkbox", { name: expected })).toBe(control);
  },
);

test("keeps the custom element description alongside a UI SDK field description", async () => {
  const fixture = (
    <Field>
      <FieldLabel>Field label</FieldLabel>
      <FieldDescription>Field description.</FieldDescription>
      <span id="extra-description">Extra description.</span>
      <Checkbox
        label="Visible label"
        render={<div aria-describedby="extra-description" />}
      />
    </Field>
  );
  const container = serverRender(fixture);
  const scope = within(container);
  const control = scope.getByRole("checkbox", {
    name: "Field label",
    description: "Extra description. Field description.",
  });

  render(fixture, { container, hydrate: true });
  await act(async () => {});

  expect(
    scope.getByRole("checkbox", {
      name: "Field label",
      description: "Extra description. Field description.",
    }),
  ).toBe(control);
});
