// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { cleanup, render, within } from "@testing-library/react";
import type * as React from "react";
import { renderToStaticMarkup, renderToString } from "react-dom/server";
import { Meter } from "./meter.tsx";
import { Progress } from "./progress.tsx";

type GaugeCase = {
  name: string;
  slot: string;
  role: string;
  label: string;
  /** Renders the gauge with the given extra props. */
  gauge: (props: {
    value: number | null;
    showValue?: boolean;
    "aria-label"?: string;
    "aria-labelledby"?: string;
  }) => React.ReactElement;
};

afterEach(cleanup);

const GAUGE_CASES: GaugeCase[] = [
  {
    name: "Progress",
    slot: "progress",
    role: "progressbar",
    label: "Uploading",
    gauge: (props) => <Progress label="Uploading" {...props} />,
  },
  {
    name: "Meter",
    slot: "meter",
    role: "meter",
    label: "Storage used",
    // The `Meter` value type excludes `null`, so the `null` case uses 0.
    gauge: ({ value, ...props }) => (
      <Meter label="Storage used" value={value ?? 0} {...props} />
    ),
  },
];

/** Returns the `aria-labelledby` tokens that no `id` in the markup defines. */
function danglingLabelledByIds(markup: string): string[] {
  const ids = new Set(
    [...markup.matchAll(/\bid="([^"]*)"/g)].map((match) => match[1]),
  );
  return [...markup.matchAll(/\baria-labelledby="([^"]*)"/g)]
    .flatMap((match) => match[1].split(/\s+/))
    .filter((token) => token !== "" && !ids.has(token));
}

/** Returns the opening tag of the first element with the given `data-slot`. */
function rootTag(markup: string, slot: string): string {
  const tag = [...markup.matchAll(/<[^>]*>/g)]
    .map((match) => match[0])
    .find((candidate) => candidate.includes(`data-slot="${slot}"`));
  if (tag === undefined) {
    throw new Error(`No element with data-slot="${slot}"`);
  }
  return tag;
}

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

describe.each(GAUGE_CASES)("$name static markup name", (gaugeCase) => {
  const { slot, role, label, gauge } = gaugeCase;

  for (const showValue of [false, true]) {
    for (const value of [40, null]) {
      test(`references its label for value ${value} and showValue ${showValue}`, () => {
        const markup = renderToStaticMarkup(gauge({ value, showValue }));

        expect(danglingLabelledByIds(markup)).toEqual([]);
        const labelledBy = /\baria-labelledby="([^"]+)"/.exec(
          rootTag(markup, slot),
        )?.[1];
        expect(labelledBy).toBeTruthy();
        const labelElement = new RegExp(
          `<[^>]*\\bid="${escapeRegExp(labelledBy ?? "")}"[^>]*data-slot="${slot}-label"[^>]*>${escapeRegExp(label)}<`,
        );
        const labelElementReversed = new RegExp(
          `<[^>]*data-slot="${slot}-label"[^>]*\\bid="${escapeRegExp(labelledBy ?? "")}"[^>]*>${escapeRegExp(label)}<`,
        );
        expect(
          labelElement.test(markup) || labelElementReversed.test(markup),
        ).toBe(true);
      });
    }
  }

  test("keeps a caller aria-labelledby on the root", () => {
    const markup = renderToStaticMarkup(
      gauge({ value: 40, "aria-labelledby": "external" }),
    );
    const root = rootTag(markup, slot);

    expect(root).toContain('aria-labelledby="external"');
    expect(root.match(/aria-labelledby=/g)).toHaveLength(1);
  });

  test("keeps an explicit aria-label without a label reference in static markup", () => {
    const markup = renderToStaticMarkup(
      gauge({ value: 40, "aria-label": "Custom name", "aria-labelledby": " " }),
    );
    const root = rootTag(markup, slot);

    expect(root).toContain('aria-label="Custom name"');
    expect(root).not.toMatch(/aria-labelledby="[^"]+"/);
    expect(markup).toContain(`>${label}<`);
  });

  test.each([
    { scenario: "no caller name", expected: label },
    { scenario: "blank aria-label", ariaLabel: " ", expected: label },
    {
      scenario: "blank caller names",
      ariaLabel: " ",
      ariaLabelledBy: " ",
      expected: label,
    },
    {
      scenario: "explicit aria-label",
      ariaLabel: "Custom name",
      expected: "Custom name",
    },
    {
      scenario: "both caller names",
      ariaLabel: "Custom name",
      ariaLabelledBy: "external",
      expected: "External name",
    },
  ])(
    "names the gauge before and after hydration with $scenario",
    ({ ariaLabel, ariaLabelledBy, expected }) => {
      const fixture = (
        <>
          <span id="external">External name</span>
          {gauge({
            value: 40,
            "aria-label": ariaLabel,
            "aria-labelledby": ariaLabelledBy,
          })}
        </>
      );
      const container = document.createElement("div");
      container.innerHTML = renderToString(fixture);
      document.body.append(container);
      const serverGauge = within(container).queryByRole(role, {
        name: expected,
      });

      render(fixture, { container, hydrate: true });

      expect(serverGauge).toBeTruthy();
      expect(within(container).getByRole(role, { name: expected })).toBe(
        serverGauge,
      );
      expect(within(container).getByText(label)).toBeTruthy();
    },
  );
});

describe("gauge name ids", () => {
  test("two gauges in one render get distinct label references", () => {
    const markup = renderToStaticMarkup(
      <>
        <Meter label="Storage used" value={40} />
        <Progress label="Uploading" value={40} />
      </>,
    );
    const references = [...markup.matchAll(/\baria-labelledby="([^"]+)"/g)].map(
      (match) => match[1],
    );

    expect(references).toHaveLength(2);
    expect(references[0]).not.toBe(references[1]);
    expect(danglingLabelledByIds(markup)).toEqual([]);
  });
});
