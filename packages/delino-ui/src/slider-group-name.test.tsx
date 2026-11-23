// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { cleanup, render, screen } from "@testing-library/react";
import { renderToStaticMarkup } from "react-dom/server";
import { Slider } from "./slider.tsx";

afterEach(cleanup);

/** Returns the opening tag of the first element with the given `data-slot`. */
function openingTag(markup: string, slot: string): string {
  const tag = [...markup.matchAll(/<[^>]*>/g)]
    .map((match) => match[0])
    .find((candidate) => candidate.includes(`data-slot="${slot}"`));
  if (tag === undefined) {
    throw new Error(`No element with data-slot="${slot}"`);
  }
  return tag;
}

function attribute(tag: string, name: string): string | undefined {
  return new RegExp(`\\b${name}="([^"]*)"`).exec(tag)?.[1];
}

/** Returns the id tokens of `attributeName` that no `id` in the markup defines. */
function danglingIds(markup: string, attributeName: string): string[] {
  const ids = new Set(
    [...markup.matchAll(/\bid="([^"]*)"/g)].map((match) => match[1]),
  );
  return [...markup.matchAll(new RegExp(`\\b${attributeName}="([^"]*)"`, "g"))]
    .flatMap((match) => match[1].split(/\s+/))
    .filter((token) => token !== "" && !ids.has(token));
}

/** Returns the text of the element with the given id. */
function textOfId(markup: string, id: string): string | undefined {
  const escaped = id.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`<[^>]*\\bid="${escaped}"[^>]*>([^<]*)<`).exec(markup)?.[1];
}

describe("Slider range group name in static markup", () => {
  test("references the visible label, the description and the thumb names", () => {
    const markup = renderToStaticMarkup(
      <Slider label="Price" defaultValue={[20, 80]} description="Per night" />,
    );

    const group = openingTag(markup, "slider");
    const labelledBy = attribute(group, "aria-labelledby");
    expect(labelledBy).toBeTruthy();
    expect(textOfId(markup, labelledBy ?? "")).toBe("Price");
    const describedBy = attribute(group, "aria-describedby");
    expect(textOfId(markup, describedBy ?? "")).toBe("Per night");
    expect(danglingIds(markup, "aria-labelledby")).toEqual([]);
    expect(danglingIds(markup, "aria-describedby")).toEqual([]);
    // Each thumb still references the label and its position text.
    const thumbs = [...markup.matchAll(/<input[^>]*>/g)]
      .map((match) => match[0])
      .filter((tag) => tag.includes('type="range"'));
    expect(thumbs.length).toBe(2);
    for (const [index, thumb] of thumbs.entries()) {
      const tokens = (attribute(thumb, "aria-labelledby") ?? "").split(" ");
      expect(tokens[0]).toBe(labelledBy);
      expect(textOfId(markup, tokens[1])).toBe(`${index + 1} of 2`);
    }
  });

  test("gives a caller aria-labelledby precedence", () => {
    const markup = renderToStaticMarkup(
      <>
        <span id="budget-label">Budget</span>
        <Slider
          label="Price"
          defaultValue={[20, 80]}
          aria-labelledby="budget-label"
        />
      </>,
    );

    expect(attribute(openingTag(markup, "slider"), "aria-labelledby")).toBe(
      "budget-label",
    );
    expect(danglingIds(markup, "aria-labelledby")).toEqual([]);
  });

  test("does not point the group at the label next to a caller aria-label", () => {
    const markup = renderToStaticMarkup(
      <Slider label="Price" defaultValue={[20, 80]} aria-label="Nightly" />,
    );

    // The UI SDK still adds its own predicted reference, which no element
    // has, so the name comes from `aria-label`. The wrapper must not add the
    // label id, because `aria-labelledby` would override `aria-label`.
    const group = openingTag(markup, "slider");
    expect(attribute(group, "aria-label")).toBe("Nightly");
    const labelId = attribute(openingTag(markup, "field-label"), "id");
    expect(labelId).toBeTruthy();
    expect(attribute(group, "aria-labelledby")).not.toBe(labelId);
  });

  test("keeps the single-thumb group reference of the UI SDK label", () => {
    const markup = renderToStaticMarkup(
      <Slider label="Volume" defaultValue={40} />,
    );

    expect(danglingIds(markup, "aria-labelledby")).toEqual([]);
    expect(attribute(openingTag(markup, "slider"), "aria-labelledby")).toBe(
      undefined,
    );
  });
});

describe("Slider range group name after mount", () => {
  test("names the group by the label and keeps the thumb names and description", () => {
    render(
      <Slider label="Price" defaultValue={[20, 80]} description="Per night" />,
    );

    const group = screen.getByRole("group", { name: "Price" });
    expect(group.getAttribute("aria-labelledby")).toBeTruthy();
    expect(screen.getByRole("group", { description: "Per night" })).toBe(group);
    expect(screen.getByRole("slider", { name: "Price 1 of 2" })).toBeTruthy();
    expect(screen.getByRole("slider", { name: "Price 2 of 2" })).toBeTruthy();
  });

  test("gives a caller aria-labelledby precedence", () => {
    render(
      <>
        <span id="budget-label">Budget</span>
        <Slider
          label="Price"
          defaultValue={[20, 80]}
          aria-labelledby="budget-label"
        />
      </>,
    );

    expect(screen.getByRole("group", { name: "Budget" })).toBeTruthy();
    expect(screen.queryByRole("group", { name: "Price" })).toBeNull();
    expect(screen.getByRole("slider", { name: "Price 1 of 2" })).toBeTruthy();
  });

  test("keeps the name of a caller aria-label", () => {
    render(
      <Slider label="Price" defaultValue={[20, 80]} aria-label="Nightly" />,
    );

    expect(screen.getByRole("group", { name: "Nightly" })).toBeTruthy();
  });

  test("keeps the name of a single thumb", () => {
    render(<Slider label="Volume" defaultValue={40} />);

    expect(screen.getByRole("slider", { name: "Volume" })).toBeTruthy();
  });
});
