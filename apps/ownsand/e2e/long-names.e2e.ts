// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect, type Screen } from "e2e";

// The suite runs against the in-browser fake control plane (see
// e2e.config.ts). The ID of the seeded organization comes from
// src/mock/mock-api.ts.
const ACME = "0193a000-0000-7000-8000-000000000001";
const BUILD_BOX = "0193a000-0000-7000-8000-0000000000d1";
const LONG_DEVICE_NAME = "w".repeat(253);

// An unbroken name of the longest length that the name fields accept. Each
// test uses its own prefix, so the page finds its row by the title text.
const NAME_LENGTH = 100;
function longName(prefix: string): string {
  return prefix + "a".repeat(NAME_LENGTH - prefix.length);
}

// Which element of a list row follows the title: the action button of a
// template or API key row, or the role badge of an organization row.
enum Neighbor {
  Button = "button",
  Badge = "badge",
}

// A type alias, not an interface, so the object is a JSON value that
// `browser.evaluate` can return.
type RowGeometry = {
  found: boolean;
  // The title text is wider than the title box, so it spills over its
  // neighbors.
  titleOverflows: boolean;
  // The right edge of the title box and the left edge of its neighbor.
  titleRight: number;
  neighborLeft: number;
  // The right edges of the neighbor and of the row that holds both.
  neighborRight: number;
  rowRight: number;
  rowHeight: number;
};

interface Browser {
  evaluate<
    T extends import("e2e").JsonValue,
    Arg extends import("e2e").JsonValue,
  >(
    fn: (arg: Arg) => T,
    arg: Arg,
  ): Promise<T>;
}

// Measures the row whose `.list-title` starts with `prefix`.
function measureRow(
  browser: Browser,
  prefix: string,
  neighbor: Neighbor,
): Promise<RowGeometry> {
  return browser.evaluate(
    (arg: { prefix: string; neighbor: string }): RowGeometry => {
      const empty: RowGeometry = {
        found: false,
        titleOverflows: false,
        titleRight: 0,
        neighborLeft: 0,
        neighborRight: 0,
        rowRight: 0,
        rowHeight: 0,
      };
      const title = Array.from(document.querySelectorAll(".list-title")).find(
        (el) => (el.textContent ?? "").startsWith(arg.prefix),
      );
      const row = title?.closest(".list-row");
      if (!title || !row) return empty;
      const next =
        arg.neighbor === "badge"
          ? title.nextElementSibling
          : row.querySelector("button");
      if (!next) return empty;
      const titleBox = title.getBoundingClientRect();
      const nextBox = next.getBoundingClientRect();
      const rowBox = row.getBoundingClientRect();
      return {
        found: true,
        titleOverflows: title.scrollWidth > title.clientWidth + 0.5,
        titleRight: titleBox.right,
        neighborLeft: nextBox.left,
        neighborRight: nextBox.right,
        rowRight: rowBox.right,
        rowHeight: rowBox.height,
      };
    },
    { prefix, neighbor },
  );
}

async function expectRowContained(
  browser: Browser,
  prefix: string,
  neighbor: Neighbor,
): Promise<void> {
  const m = await measureRow(browser, prefix, neighbor);
  expect(m.found, "the row is on the page").toBe(true);
  expect(m.titleOverflows, "the title text fits its own box").toBe(false);
  expect(
    m.titleRight,
    "the title ends before its neighbor",
  ).toBeLessThanOrEqual(m.neighborLeft + 0.5);
  expect(
    m.neighborRight,
    "the neighbor stays inside the row",
  ).toBeLessThanOrEqual(m.rowRight + 0.5);
}

async function documentWidths(
  browser: Browser,
): Promise<{ scrollWidth: number; clientWidth: number }> {
  return browser.evaluate(
    (_: null) => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }),
    null,
  );
}

// The number of text lines of the title that starts with `prefix`.
function lineCount(browser: Browser, prefix: string): Promise<number> {
  return browser.evaluate((wanted: string) => {
    const title = Array.from(document.querySelectorAll(".list-title")).find(
      (el) => (el.textContent ?? "").startsWith(wanted),
    );
    if (!title) return 0;
    const range = document.createRange();
    range.selectNodeContents(title);
    return new Set(
      Array.from(range.getClientRects()).map((r) => Math.round(r.top)),
    ).size;
  }, prefix);
}

async function signIn(screen: Screen, open: () => Promise<void>) {
  await open();
  await screen.getByRole("button", "Sign in with Delino").tap();
}

describe("long names", () => {
  for (const { width, name } of [
    { width: 320, name: LONG_DEVICE_NAME },
    { width: 761, name: LONG_DEVICE_NAME },
    { width: 320, name: "build-box-1" },
  ]) {
    const long = name === LONG_DEVICE_NAME;
    const scenario = `${long ? "long" : "short"}-device-name-${width}px`;
    test(`contains the ${scenario} detail heading and controls`, async ({
      app,
      screen,
      browser,
    }) => {
      await browser.setViewport({ width, height: 800 });
      await signIn(screen, () => app.open("/"));
      if (long) {
        await browser.evaluate((deviceName) => {
          sessionStorage.setItem("ownsand-e2e-device-name", deviceName);
          return null;
        }, name);
      }
      await app.open(`/orgs/${ACME}/devices/${BUILD_BOX}`);

      await expect(
        screen.getByRole("heading", name, { level: 1, exact: true }),
      ).toBeVisible();
      const status = screen.getByText("Online", { exact: true });
      const save = screen.getByRole("button", "Save limits");
      const remove = screen.getByRole("button", "Remove device");
      await expect(status).toBeVisible();
      await expect(save).toBeEnabled();
      await expect(remove).toBeEnabled();
      await app.screenshot(scenario);

      const { lines, headerFits, textFits } = await browser.evaluate(() => {
        const header = document.querySelector(".page-header")!;
        const heading = header.querySelector("h1")!;
        const badge = header.querySelector(".dui-badge")!;
        const headerBox = header.getBoundingClientRect();
        const headingBox = heading.getBoundingClientRect();
        const badgeBox = badge.getBoundingClientRect();
        const range = document.createRange();
        range.selectNodeContents(heading);
        const rects = Array.from(range.getClientRects());
        return {
          lines: new Set(rects.map((rect) => rect.top)).size,
          headerFits: [headingBox, badgeBox].every(
            (box) => box.left >= headerBox.left && box.right <= headerBox.right,
          ),
          textFits: rects.every(
            (rect) =>
              rect.left >= headingBox.left &&
              rect.right <= headingBox.right + 0.5,
          ),
        };
      });
      const { scrollWidth, clientWidth } = await documentWidths(browser);
      expect(scrollWidth).toBeLessThanOrEqual(clientWidth);
      expect(headerFits).toBe(true);
      expect(textFits).toBe(true);
      if (long) expect(lines).toBeGreaterThan(1);
      else expect(lines).toBe(1);

      await remove.tap();
      await expect(screen.getByRole("alertdialog")).toBeVisible();
    });
  }

  test("wraps an unbroken template name away from the Delete button", async ({
    app,
    screen,
    browser,
  }) => {
    const name = longName("TPLNAME_");
    await signIn(screen, () => app.open("/"));
    await app.open(`/orgs/${ACME}/snapshots`);
    // The row button is named by its row ("Make template from snapshot ..."),
    // so match the start of the name.
    await screen
      .getByRole("button", "Make template", { exact: false })
      .first()
      .tap();
    await expect(screen.getByLabel("Template name")).toBeFocused();
    await browser.keyboard.type(name);
    await screen.getByRole("button", "Create template").tap();
    await expect(screen.getByText(name)).toBeVisible();

    await expectRowContained(browser, "TPLNAME_", Neighbor.Button);
  });

  test("wraps an unbroken API key name away from the Revoke button", async ({
    app,
    screen,
    browser,
  }) => {
    const name = longName("KEYNAME_");
    await signIn(screen, () => app.open("/"));
    await app.open(`/orgs/${ACME}/api-keys`);
    await screen.getByRole("button", "Create API key").tap();
    await expect(screen.getByLabel("Name")).toBeFocused();
    await browser.keyboard.type(name);
    await screen.getByRole("button", "Create key").tap();
    await browser.keyboard.press("Escape");
    await expect(screen.getByText(`${name} · Active`)).toBeVisible();

    await expectRowContained(browser, "KEYNAME_", Neighbor.Button);
  });

  test("keeps an unbroken organization name inside the list row and the navigation header", async ({
    app,
    screen,
    browser,
  }) => {
    const name = longName("ORGNAME_");
    await signIn(screen, () => app.open("/"));
    await screen.getByRole("button", "Create organization").tap();
    await expect(screen.getByLabel("Organization name")).toBeFocused();
    await browser.keyboard.type(name);
    await screen.getByRole("button", "Create").tap();
    await expect(browser).toHaveURL(/\/orgs\/[0-9a-f-]+\/sandboxes$/);

    // The navigation header: the name wraps inside the 220 px column, so the
    // role badge stays in the column and the page does not widen.
    for (const width of [1280, 800, 761]) {
      await browser.setViewport({ width, height: 800 });
      await expect(
        screen.getByRole("heading", "Sandboxes", { level: 1 }),
      ).toBeVisible();
      const m = await browser.evaluate((_: null) => {
        const org = document.querySelector(".sidenav-org");
        return {
          found: org !== null,
          scrollWidth: org ? org.scrollWidth : 0,
          clientWidth: org ? org.clientWidth : 0,
        };
      }, null);
      expect(m.found, `${width}px: the navigation header exists`).toBe(true);
      expect(
        m.scrollWidth,
        `${width}px: the header text fits its box`,
      ).toBeLessThanOrEqual(m.clientWidth);
      const d = await documentWidths(browser);
      expect(
        d.scrollWidth,
        `${width}px: the page does not scroll sideways`,
      ).toBeLessThanOrEqual(d.clientWidth);
    }

    // The organization list: go back to it through the brand link, which
    // keeps the fake control plane state of this page load.
    await browser.setViewport({ width: 1280, height: 800 });
    await screen.getByRole("link", "OwnSand").tap();
    await expect(
      screen.getByRole("heading", "Organizations", { level: 1 }),
    ).toBeVisible();
    await expectRowContained(browser, "ORGNAME_", Neighbor.Badge);
  });

  test("keeps short and spaced names on one line at the same row height", async ({
    app,
    screen,
    browser,
  }) => {
    await signIn(screen, () => app.open("/"));
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    const org = await measureRow(browser, "Acme Robotics", Neighbor.Badge);
    expect(org.found).toBe(true);
    expect(await lineCount(browser, "Acme Robotics")).toBe(1);
    // The row keeps its 64 px minimum height.
    expect(org.rowHeight).toBeLessThanOrEqual(64.5);

    await app.open(`/orgs/${ACME}/snapshots`);
    await expect(screen.getByText("node-24-toolchain")).toBeVisible();
    const template = await measureRow(
      browser,
      "node-24-toolchain",
      Neighbor.Button,
    );
    expect(template.found).toBe(true);
    expect(await lineCount(browser, "node-24-toolchain")).toBe(1);
    // The title and the details line fit the two-line row of the base layout.
    expect(template.rowHeight).toBeLessThan(80);
  });
});
