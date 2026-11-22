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
  over(foreground: Rgba, background: Rgba): Rgba;
  stops(image: string): Rgba[];
  ratio(foreground: Rgba, background: Rgba): number;
}

describe("home", () => {
  test("shows the hero logo", async ({ app, screen }) => {
    await app.open("/");

    // The Rspress home hero renders its name as text, not as a heading, so
    // the logo image is the stable hero landmark.
    await expect(screen.getByRole("image", "Delino logo")).toBeVisible();
  });

  test("links the docs action to the documentation site", async ({
    app,
    screen,
  }) => {
    await app.open("/");

    await expect(screen.getByRole("link", "Read the docs")).toHaveAttribute(
      "href",
      "https://docs.delino.io",
    );
  });

  test("gives every feature card a distinct title", async ({
    app,
    browser,
  }) => {
    await app.open("/");

    // Rspress keys the feature cards by title, so a repeated title makes React
    // report duplicate keys in the development console and can drop a card.
    const titles = await browser.evaluate(() =>
      Array.from(document.querySelectorAll(".rp-home-feature__title")).map(
        (element) => element.textContent?.trim() ?? "",
      ),
    );

    expect(titles).toHaveLength(3);
    expect(new Set(titles).size).toBe(titles.length);
  });

  test(
    "presents the product features",
    { tags: ["agent"] },
    async ({ app, agent }) => {
      await app.open("/");

      await agent.assert("the page lists three feature cards");
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
      // Composites a translucent foreground over an opaque background.
      const over = (foreground: Rgba, background: Rgba): Rgba => ({
        r: foreground.r * foreground.a + background.r * (1 - foreground.a),
        g: foreground.g * foreground.a + background.g * (1 - foreground.a),
        b: foreground.b * foreground.a + background.b * (1 - foreground.a),
        a: 1,
      });
      // Reads the color stops of a computed `linear-gradient(...)`.
      const stops = (image: string): Rgba[] => {
        const found =
          image.match(/rgba?\([^)]*\)|color\(srgb [^)]*\)/g)?.map(parse) ?? [];
        if (found.length < 2) {
          throw new Error(`Expected a gradient with color stops, got ${image}`);
        }
        return found;
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
      const kit: ColorKit = { parse, resolve, over, stops, ratio };
      (window as unknown as { colorKit: ColorKit }).colorKit = kit;
      return true;
    };

  // Adds a minimal overview group, which Rspress renders on its overview
  // pages, because neither app has such a page. It reuses the Rspress class
  // names, so the real Rspress and theme styles apply.
  const addOverviewGroup = () =>
    function () {
      const group = document.createElement("div");
      group.style.cssText =
        "position:fixed;top:80px;left:200px;right:200px;z-index:9999;padding:24px;background:var(--rp-c-bg)";
      group.innerHTML =
        '<div class="rp-overview-group"><div class="rp-overview-group__item">' +
        '<div class="rp-overview-group__item__title">' +
        '<a class="rp-overview-group__item__title__link" href="#">Overview group</a>' +
        "</div></div></div>";
      document.body.append(group);
      return true;
    };

  // Reads the hovered overview group title: its link text color against the
  // translucent hover background composited over the page surfaces.
  const readOverviewHover = () =>
    function () {
      const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
      const link = document.querySelector(
        ".rp-overview-group__item__title__link",
      );
      const title = document.querySelector(".rp-overview-group__item__title");
      if (!link || !title) {
        throw new Error("The overview group is missing");
      }
      const text = kit.parse(getComputedStyle(link).color);
      const hover = kit.parse(getComputedStyle(title).backgroundColor);
      const expected = kit.resolve("--rp-c-overview-group-bg-hover");
      const key = ({ r, g, b, a }: Rgba) =>
        [r, g, b, a].map((value) => value.toFixed(3)).join(",");
      return {
        // True once the title background transition reached its end value.
        settled: link.matches(":hover") && key(hover) === key(expected),
        textMatchesBrand: key(text) === key(kit.resolve("--rp-c-brand")),
        ratioOverBg: kit.ratio(text, kit.over(hover, kit.resolve("--rp-c-bg"))),
        ratioOverBgSoft: kit.ratio(
          text,
          kit.over(hover, kit.resolve("--rp-c-bg-soft")),
        ),
      };
    };

  // Reads the hero button and wordmark gradients: the white button label
  // against every button stop, and the wordmark stops against the page
  // background and the hero glow behind the wordmark.
  const readHeroContrast = () =>
    function (glow: Rgba) {
      const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
      const button = document.querySelector(".rp-button--brand");
      const wordmark = document.querySelector(".rp-home-hero__title-brand");
      if (!button || !wordmark) {
        throw new Error("The hero button or the hero wordmark is missing");
      }
      const label = kit.parse(getComputedStyle(button).color);
      const background = kit.resolve("--rp-c-bg");
      const wordmarkStops = kit.stops(
        getComputedStyle(wordmark).backgroundImage,
      );
      return {
        label: [label.r, label.g, label.b].join(","),
        buttonLabelLowest: Math.min(
          ...kit
            .stops(getComputedStyle(button).backgroundImage)
            .map((stop) => kit.ratio(label, stop)),
        ),
        wordmarkBackgroundLowest: Math.min(
          ...wordmarkStops.map((stop) => kit.ratio(stop, background)),
        ),
        wordmarkGlowLowest: Math.min(
          ...wordmarkStops.map((stop) => kit.ratio(stop, glow)),
        ),
      };
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
    await app.open("/");
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
    await app.open("/");
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

  // Rspress mixes a translucent hover background into the overview group
  // title and paints the hovered title text with `--rp-c-brand`.
  for (const mode of ["light", "dark"] as const) {
    test(`keeps the hovered overview group title at 4.5:1 in ${mode} mode`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/");
      await browser.evaluate(installColorKit());
      if (mode === "dark") {
        await browser.evaluate(() => {
          document.documentElement.classList.add("rp-dark");
          return true;
        });
      }
      await browser.evaluate(addOverviewGroup());

      await screen.getByRole("link", "Overview group").hover();
      await expect
        .poll(async () => (await browser.evaluate(readOverviewHover())).settled)
        .toBe(true);
      const hovered = await browser.evaluate(readOverviewHover());

      expect(hovered.textMatchesBrand).toBe(true);
      expect(hovered.ratioOverBg).toBeGreaterThanOrEqual(4.5);
      expect(hovered.ratioOverBgSoft).toBeGreaterThanOrEqual(4.5);
    });
  }

  // The glow is the darkest (dark) or lightest (light) color that the hero
  // background gradients paint behind the wordmark, sampled from the rendered
  // page: rgb(33, 25, 81) in dark mode and rgb(231, 232, 255) in light mode.
  for (const mode of ["light", "dark"] as const) {
    const glow: Rgba =
      mode === "dark"
        ? { r: 33, g: 25, b: 81, a: 1 }
        : { r: 231, g: 232, b: 255, a: 1 };

    test(`keeps the hero button label at 4.5:1 along its gradient in ${mode} mode`, async ({
      app,
      browser,
    }) => {
      await app.open("/");
      await browser.evaluate(installColorKit());
      if (mode === "dark") {
        await browser.evaluate(() => {
          document.documentElement.classList.add("rp-dark");
          return true;
        });
      }

      const hero = await browser.evaluate(readHeroContrast(), glow);

      expect(hero.label).toBe("255,255,255");
      expect(hero.buttonLabelLowest).toBeGreaterThanOrEqual(4.5);
    });

    test(`keeps the hero wordmark gradient at 3:1 in ${mode} mode`, async ({
      app,
      browser,
    }) => {
      await app.open("/");
      await browser.evaluate(installColorKit());
      if (mode === "dark") {
        await browser.evaluate(() => {
          document.documentElement.classList.add("rp-dark");
          return true;
        });
      }

      const hero = await browser.evaluate(readHeroContrast(), glow);

      expect(hero.wordmarkBackgroundLowest).toBeGreaterThanOrEqual(3);
      expect(hero.wordmarkGlowLowest).toBeGreaterThanOrEqual(3);
    });
  }
});
