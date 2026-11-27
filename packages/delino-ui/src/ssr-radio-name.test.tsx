// SPDX-License-Identifier: Apache-2.0
import { afterEach, expect, test } from "@rstest/core";
import {
  act,
  cleanup,
  fireEvent,
  render,
  within,
} from "@testing-library/react";
import { Field, FieldDescription } from "@ui-sdk/react/field";
import { renderToString } from "react-dom/server";
import { RadioGroup, RadioGroupItem } from "./radio-group.tsx";

afterEach(cleanup);

test("names each custom radio from its own visible label before and after hydration", async () => {
  const fixture = (
    <RadioGroup label="Plan" defaultValue="monthly">
      <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
      <RadioGroupItem value="yearly" render={<div />}>
        Yearly
      </RadioGroupItem>
      <RadioGroupItem value="weekly" render={(props) => <div {...props} />}>
        Weekly
      </RadioGroupItem>
    </RadioGroup>
  );
  const container = document.createElement("div");
  container.innerHTML = renderToString(fixture);
  document.body.append(container);
  const group = within(container);
  const monthly = group.getByRole("radio", { name: "Monthly" });
  const yearly = group.getByRole("radio", { name: "Yearly" });
  const weekly = group.getByRole("radio", { name: "Weekly" });
  const yearlyLabel = group.getByText("Yearly");
  const weeklyLabel = group.getByText("Weekly");

  expect(monthly.tagName).toBe("BUTTON");
  expect(yearly.tagName).toBe("DIV");
  expect(weekly.tagName).toBe("DIV");
  expect(yearlyLabel.id).toBeTruthy();
  expect(weeklyLabel.id).toBeTruthy();
  expect(yearlyLabel.id).not.toBe(weeklyLabel.id);
  const yearlyId = yearlyLabel.id;
  const weeklyId = weeklyLabel.id;
  expect(yearly.getAttribute("aria-labelledby")).toBe(yearlyId);
  expect(weekly.getAttribute("aria-labelledby")).toBe(weeklyId);

  render(fixture, { container, hydrate: true });
  await act(async () => {});

  expect(group.getByRole("radio", { name: "Monthly" })).toBe(monthly);
  expect(group.getByRole("radio", { name: "Yearly" })).toBe(yearly);
  expect(group.getByRole("radio", { name: "Weekly" })).toBe(weekly);
  expect(yearly.getAttribute("aria-labelledby")).toBe(yearlyId);
  expect(weekly.getAttribute("aria-labelledby")).toBe(weeklyId);
  expect(document.getElementById(yearlyId)).toBe(yearlyLabel);
  expect(document.getElementById(weeklyId)).toBe(weeklyLabel);
});

test("keeps explicit names on the radio props and on elements and callbacks", async () => {
  const fixture = (
    <>
      <span id="quarterly-name">Quarterly plan</span>
      <span id="daily-name">Daily plan</span>
      <RadioGroup label="Plan">
        <RadioGroupItem
          value="yearly"
          render={<div />}
          aria-label="Annual plan"
        >
          Yearly
        </RadioGroupItem>
        <RadioGroupItem
          value="quarterly"
          render={<div />}
          aria-labelledby="quarterly-name"
        >
          Quarterly
        </RadioGroupItem>
        <RadioGroupItem
          value="weekly"
          render={<div aria-label="Weekly plan" />}
        >
          Weekly
        </RadioGroupItem>
        <RadioGroupItem
          value="daily"
          render={<div aria-labelledby="daily-name" />}
        >
          Daily
        </RadioGroupItem>
        <RadioGroupItem
          value="trial"
          render={(props) => <div {...props} aria-label="Trial plan" />}
        >
          Trial
        </RadioGroupItem>
      </RadioGroup>
    </>
  );
  const container = document.createElement("div");
  container.innerHTML = renderToString(fixture);
  document.body.append(container);
  const scope = within(container);
  const names = [
    "Annual plan",
    "Quarterly plan",
    "Weekly plan",
    "Daily plan",
    "Trial plan",
  ];
  const radios = names.map((name) => scope.getByRole("radio", { name }));

  render(fixture, { container, hydrate: true });
  await act(async () => {});

  expect(names.map((name) => scope.getByRole("radio", { name }))).toEqual(
    radios,
  );
});

test("keeps the render element and field descriptions on a custom radio", () => {
  const { container } = render(
    <>
      <span id="billing-period">Billed once a year.</span>
      <RadioGroup label="Plan">
        <Field>
          <RadioGroupItem
            value="yearly"
            render={<div aria-describedby="billing-period" />}
          >
            Yearly
          </RadioGroupItem>
          <FieldDescription>Available for annual contracts.</FieldDescription>
        </Field>
      </RadioGroup>
    </>,
  );

  expect(
    within(container).getByRole("radio", {
      name: "Yearly",
      description: "Billed once a year. Available for annual contracts.",
    }),
  ).toBeTruthy();
});

test("preserves keyboard, form and label interactions after hydration", async () => {
  const values: (string | null)[] = [];
  const fixture = (
    <form aria-label="Billing">
      <RadioGroup
        label="Plan"
        name="plan"
        defaultValue="monthly"
        onValueChange={(value) => values.push(value)}
      >
        <RadioGroupItem value="monthly">Monthly</RadioGroupItem>
        <RadioGroupItem value="yearly" render={<div />}>
          Yearly
        </RadioGroupItem>
      </RadioGroup>
    </form>
  );
  const container = document.createElement("div");
  container.innerHTML = renderToString(fixture);
  document.body.append(container);
  render(fixture, { container, hydrate: true });
  await act(async () => {});

  const scope = within(container);
  const form = scope.getByRole<HTMLFormElement>("form", { name: "Billing" });
  const monthly = scope.getByRole("radio", { name: "Monthly" });
  const yearly = scope.getByRole("radio", { name: "Yearly" });
  expect(new FormData(form).getAll("plan")).toEqual(["monthly"]);

  monthly.focus();
  fireEvent.keyDown(monthly, { key: "ArrowRight" });
  expect(document.activeElement).toBe(yearly);
  expect(yearly.getAttribute("aria-checked")).toBe("true");
  expect(monthly.getAttribute("aria-checked")).toBe("false");
  expect(values).toEqual(["yearly"]);
  expect(new FormData(form).getAll("plan")).toEqual(["yearly"]);

  fireEvent.keyDown(yearly, { key: "ArrowLeft" });
  expect(document.activeElement).toBe(monthly);
  expect(monthly.getAttribute("aria-checked")).toBe("true");
  expect(yearly.getAttribute("aria-checked")).toBe("false");
  expect(values).toEqual(["yearly", "monthly"]);
  expect(new FormData(form).getAll("plan")).toEqual(["monthly"]);

  fireEvent.click(scope.getByText("Yearly"));
  expect(document.activeElement).toBe(yearly);
  expect(yearly.getAttribute("aria-checked")).toBe("true");
  expect(monthly.getAttribute("aria-checked")).toBe("false");
  expect(values).toEqual(["yearly", "monthly", "yearly"]);
});
