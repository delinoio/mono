// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// An sRGB color with 0..255 channels and a 0..1 alpha.
type Rgba = { r: number; g: number; b: number; a: number };

// Helpers that `installColorKit` attaches to `window`. `browser.evaluate`
// serializes one function, so a test that needs color math installs the kit
// first and reads it from the page in the function that it evaluates.
interface ColorKit {
  parse(color: string): Rgba;
  resolve(name: string): Rgba;
  ratio(foreground: Rgba, background: Rgba): number;
}

describe("guide", () => {
  test("opens the introduction from the home page", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");

    await screen.getByRole("link", "Get started").tap();

    await expect(browser).toHaveURL("/guide/introduction");
    await expect(
      screen.getByRole("heading", "Introduction", { level: 1 }),
    ).toBeVisible();
  });

  test(
    "navigates to the getting started guide",
    { tags: ["agent"] },
    async ({ app, agent, browser, screen }) => {
      await app.open("/guide/introduction");

      await agent.act("open the Getting started guide");

      await expect(browser).toHaveURL("/guide/getting-started");
      await expect(
        screen.getByRole("heading", "Getting started", { level: 1 }),
      ).toBeVisible();
    },
  );
});

describe("brand colors", () => {
  // Reads the resolved brand custom properties and the Rspress dark surfaces
  // in the page, then computes WCAG 2.1 contrast ratios. Adding the `rp-dark`
  // class to `html` is what the Rspress theme switch does.
  const readBrandContrast = () =>
    function () {
      const probe = document.createElement("span");
      document.body.append(probe);
      const resolve = (name: string): [number, number, number] => {
        // An undefined custom property makes `var()` invalid at computed-value
        // time, so the probe would silently inherit the body color and could
        // pass the contrast check. Fail on an undefined variable instead.
        if (
          !getComputedStyle(document.documentElement)
            .getPropertyValue(name)
            .trim()
        ) {
          throw new Error(`${name} is not defined`);
        }
        probe.style.color = "";
        probe.style.color = `var(${name})`;
        const channels = getComputedStyle(probe).color.match(/[\d.]+/g) ?? [];
        return [Number(channels[0]), Number(channels[1]), Number(channels[2])];
      };
      const luminance = ([r, g, b]: [number, number, number]) => {
        const [lr, lg, lb] = [r, g, b].map((value) => {
          const channel = value / 255;
          return channel <= 0.03928
            ? channel / 12.92
            : ((channel + 0.055) / 1.055) ** 2.4;
        });
        return 0.2126 * lr! + 0.7152 * lg! + 0.0722 * lb!;
      };
      const ratio = (foreground: string, background: string) => {
        const first = luminance(resolve(foreground));
        const second = luminance(resolve(background));
        return (
          (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05)
        );
      };
      const surfaces = [
        "--rp-c-bg",
        "--rp-c-bg-soft",
        "--rp-c-bg-mute",
        "--rp-c-bg-alt",
      ];
      const lowest = (foreground: string) =>
        Math.min(...surfaces.map((surface) => ratio(foreground, surface)));
      const result = {
        brand: resolve("--rp-c-brand").join(","),
        brandLight: resolve("--rp-c-brand-light").join(","),
        brandLighter: resolve("--rp-c-brand-lighter").join(","),
        brandDark: resolve("--rp-c-brand-dark").join(","),
        brandDarker: resolve("--rp-c-brand-darker").join(","),
        brandLowest: lowest("--rp-c-brand"),
        brandLightLowest: lowest("--rp-c-brand-light"),
        brandLighterLowest: lowest("--rp-c-brand-lighter"),
      };
      probe.remove();
      return result;
    };

  // Installs the `ColorKit` in the page. `resolve` fails on an undefined custom
  // property, for the reason that `readBrandContrast` gives.
  const installColorKit = () =>
    function () {
      const parse = (color: string): Rgba => {
        // `color-mix()` computes to `color(srgb r g b / a)` with 0..1 channels,
        // while `rgb()` and `rgba()` use 0..255 channels.
        if (color.startsWith("color(") && !color.startsWith("color(srgb ")) {
          throw new Error(`Unsupported color space in ${color}`);
        }
        const scale = color.startsWith("color(") ? 255 : 1;
        const channels = (color.match(/\d*\.?\d+(?:e-?\d+)?/gi) ?? []).map(
          Number,
        );
        return {
          r: channels[0]! * scale,
          g: channels[1]! * scale,
          b: channels[2]! * scale,
          a: channels[3] ?? 1,
        };
      };
      const resolve = (name: string): Rgba => {
        if (
          !getComputedStyle(document.documentElement)
            .getPropertyValue(name)
            .trim()
        ) {
          throw new Error(`${name} is not defined`);
        }
        const probe = document.createElement("span");
        document.body.append(probe);
        probe.style.color = `var(${name})`;
        const color = parse(getComputedStyle(probe).color);
        probe.remove();
        return color;
      };
      const luminance = ({ r, g, b }: Rgba) => {
        const [lr, lg, lb] = [r, g, b].map((value) => {
          const channel = value / 255;
          return channel <= 0.03928
            ? channel / 12.92
            : ((channel + 0.055) / 1.055) ** 2.4;
        });
        return 0.2126 * lr! + 0.7152 * lg! + 0.0722 * lb!;
      };
      const ratio = (foreground: Rgba, background: Rgba) => {
        const first = luminance(foreground);
        const second = luminance(background);
        return (
          (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05)
        );
      };
      const kit: ColorKit = { parse, resolve, ratio };
      (window as unknown as { colorKit: ColorKit }).colorKit = kit;
      return true;
    };

  // Reads the hovered 404 home link and its colors against the page background.
  const readNotFoundLink = () =>
    function () {
      const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
      const link = document.querySelector(".rp-not-found__home-link");
      if (!link) {
        throw new Error("The 404 home link is missing");
      }
      const style = getComputedStyle(link);
      const background = kit.resolve("--rp-c-bg");
      const text = kit.parse(style.color);
      const border = kit.parse(style.borderTopColor);
      const join = ({ r, g, b }: Rgba) => [r, g, b].join(",");
      return {
        hovered: link.matches(":hover"),
        color: join(text),
        borderColor: join(border),
        brandDark: join(kit.resolve("--rp-c-brand-dark")),
        brandLight: join(kit.resolve("--rp-c-brand-light")),
        textRatio: kit.ratio(text, background),
        borderRatio: kit.ratio(border, background),
      };
    };

  test("keeps brand text at 4.5:1 or more on every dark surface", async ({
    app,
    browser,
  }) => {
    await app.open("/guide/introduction");
    await browser.evaluate(() => {
      document.documentElement.classList.add("rp-dark");
      return true;
    });

    const dark = await browser.evaluate(readBrandContrast());

    expect(dark.brand).toBe("138,160,255");
    expect(dark.brandLight).toBe("163,180,255");
    expect(dark.brandLighter).toBe("192,204,255");
    expect(dark.brandLowest).toBeGreaterThanOrEqual(4.5);
    expect(dark.brandLightLowest).toBeGreaterThanOrEqual(4.5);
    expect(dark.brandLighterLowest).toBeGreaterThanOrEqual(4.5);
  });

  test("keeps the light brand and the hero gradient colors unchanged", async ({
    app,
    browser,
  }) => {
    await app.open("/guide/introduction");
    const light = await browser.evaluate(readBrandContrast());
    await browser.evaluate(() => {
      document.documentElement.classList.add("rp-dark");
      return true;
    });
    const dark = await browser.evaluate(readBrandContrast());

    expect(light.brand).toBe("59,91,253");
    expect(dark.brandDark).toBe(light.brandDark);
    expect(dark.brandDarker).toBe(light.brandDarker);
  });

  // Rspress paints the 404 home link hover (text and border) with
  // `--rp-c-brand-dark`, which stays unchanged for the hero gradient. The
  // theme overrides the hover in dark mode, so it must stay readable there.
  test("keeps the 404 home link hover readable in dark mode", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/this-route-does-not-exist");
    await browser.evaluate(installColorKit());
    await browser.evaluate(() => {
      document.documentElement.classList.add("rp-dark");
      return true;
    });

    // The link text is "Take me home"; its accessible name is the aria-label.
    await screen.getByRole("link", "go to home").hover();
    // Rspress transitions the link colors for 0.3s, so wait for the end value.
    await expect
      .poll(async () => {
        const hovered = await browser.evaluate(readNotFoundLink());
        return hovered.hovered && hovered.color === hovered.brandLight;
      })
      .toBe(true);
    const hovered = await browser.evaluate(readNotFoundLink());

    expect(hovered.color).toBe("163,180,255");
    expect(hovered.borderColor).toBe("163,180,255");
    expect(hovered.textRatio).toBeGreaterThanOrEqual(4.5);
    expect(hovered.borderRatio).toBeGreaterThanOrEqual(3);
  });

  test("keeps the 404 home link hover on the brand blue in light mode", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/this-route-does-not-exist");
    await browser.evaluate(installColorKit());

    await screen.getByRole("link", "go to home").hover();
    await expect
      .poll(async () => {
        const hovered = await browser.evaluate(readNotFoundLink());
        return hovered.hovered && hovered.color === hovered.brandDark;
      })
      .toBe(true);
    const hovered = await browser.evaluate(readNotFoundLink());

    expect(hovered.color).toBe(hovered.brandDark);
    expect(hovered.borderColor).toBe(hovered.brandDark);
    expect(hovered.textRatio).toBeGreaterThanOrEqual(4.5);
  });
});
