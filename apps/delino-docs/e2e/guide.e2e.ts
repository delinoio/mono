// SPDX-License-Identifier: Apache-2.0
import { rejects, throws } from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { describe, test, type WebFixtures } from "@e2e-dev/web";
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

describe("languages", () => {
  test("renders the Korean home page with a language menu", async ({
    app,
    browser,
    screen,
  }) => {
    // Rspress 2.1.3 moves the language menu into the hamburger menu at 1280px
    // and narrower, so use a wider viewport to reach the navbar menu.
    await browser.setViewport({ width: 1440, height: 900 });
    await app.open("/ko/");
    // The hero image text is translated, so wait for the brand wordmark.
    await expect(browser.locator(".rp-home-hero__title-brand")).toBeVisible();

    const lang = await browser.evaluate(() => document.documentElement.lang);
    expect(lang).toBe("ko");

    // The menu shows the native label of the selected language and lists the
    // other languages as links to the same page in that language.
    const menu = browser.locator(
      ".rp-nav__others > .rp-nav-menu__item > .rp-nav-menu__item__container",
    );
    await expect(menu).toHaveText("한국어");
    await menu.hover();
    await expect(screen.getByRole("link", "English")).toHaveAttribute(
      "href",
      "/",
    );
  });

  test("keeps the selected language in links to the marketing site", async ({
    app,
    browser,
  }) => {
    await app.open("/ko/");

    // The translated page keeps the English URL in its frontmatter; the theme
    // `Link` adds the language prefix. The action label is translated, so the
    // test selects the alternative action by its Rspress class name.
    await expect(
      browser.locator(".rp-home-hero__action.rp-button--alt"),
    ).toHaveAttribute("href", "https://delino.io/ko/");
  });
});

describe("brand colors", () => {
  // An undefined custom property would make the probe inherit the body color.
  // Fail before resolving the probe so that inherited color cannot pass a check.
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

  type BrandContrast = { token: string; surface: string; ratio: number };

  const readBrandContrast = () =>
    function (checkDarkContrast: boolean) {
      const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
      const join = ({ r, g, b }: Rgba) => [r, g, b].join(",");
      const brand = kit.resolve("--rp-c-brand");
      const brandLight = kit.resolve("--rp-c-brand-light");
      const brandLighter = kit.resolve("--rp-c-brand-lighter");
      const palette = {
        brand: join(brand),
        brandLight: join(brandLight),
        brandLighter: join(brandLighter),
        brandDark: join(kit.resolve("--rp-c-brand-dark")),
        brandDarker: join(kit.resolve("--rp-c-brand-darker")),
      };
      if (!checkDarkContrast) {
        return { ...palette, contrasts: [] };
      }

      const surfaces = [
        "--rp-c-bg",
        "--rp-c-bg-soft",
        "--rp-c-bg-mute",
        "--rp-c-bg-alt",
      ].map((surface) => {
        const background = kit.resolve(surface);
        if (background.a !== 1) {
          throw new Error(
            `${surface} must be opaque, got alpha ${background.a}`,
          );
        }
        return { surface, background };
      });
      const tokens = [
        { token: "--rp-c-brand", foreground: brand },
        { token: "--rp-c-brand-light", foreground: brandLight },
        { token: "--rp-c-brand-lighter", foreground: brandLighter },
      ];
      const contrasts: BrandContrast[] = tokens.flatMap(
        ({ token, foreground }) =>
          surfaces.map(({ surface, background }) => ({
            token,
            surface,
            ratio: kit.ratio(kit.over(foreground, background), background),
          })),
      );
      return { ...palette, contrasts };
    };

  const expectDarkBrandContrast = ({
    token,
    surface,
    ratio,
  }: BrandContrast) => {
    expect(ratio, `${token} over ${surface}`).toBeGreaterThanOrEqual(4.5);
  };

  const setBrandMode = (browser: WebFixtures["browser"], dark: boolean) =>
    browser.evaluate((dark) => {
      document.documentElement.classList.toggle("rp-dark", dark);
      return true;
    }, dark);

  const openBrandGuide = async ({
    app,
    browser,
  }: Pick<WebFixtures, "app" | "browser">) => {
    await app.open("/guide/introduction");
    await browser.evaluate(installColorKit());
  };

  const readSearchResults = () =>
    function () {
      const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
      return Array.from(
        document.querySelectorAll(".rp-suggest-item__title"),
        (title) => {
          const link = title.closest(".rp-suggest-item__link");
          if (!link) {
            throw new Error("The search result title has no row link");
          }
          const foreground = getComputedStyle(title).color;
          const background = getComputedStyle(link).backgroundColor;
          const surface = kit.over(
            kit.parse(background),
            kit.resolve("--rp-c-bg"),
          );
          return {
            title: title.textContent?.trim(),
            current: Boolean(link.closest(".rp-suggest-item--current")),
            foreground,
            background,
            contrast: kit.ratio(
              kit.over(kit.parse(foreground), surface),
              surface,
            ),
          };
        },
      );
    };

  for (const mode of ["light", "dark"] as const) {
    test(`keeps search result titles readable in ${mode} mode`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/");
      await browser.evaluate(installColorKit());
      await browser.evaluate((dark) => {
        document.documentElement.classList.toggle("rp-dark", dark);
        return true;
      }, mode === "dark");

      await screen.getByRole("button", /^Search/).tap();
      await screen.getByRole("textbox", "SearchPanelInput").fill("placeholder");
      const readResults = () => browser.evaluate(readSearchResults());
      await expect
        .poll(async () => {
          const titles = (await readResults()).map((result) => result.title);
          return (
            titles.includes("Introduction") &&
            titles.includes("Getting started")
          );
        })
        .toBe(true);

      const before = await readResults();
      const initialIndex = before.findIndex((result) => result.current);
      expect(initialIndex).toBeGreaterThanOrEqual(0);
      const nextIndex = (initialIndex + 1) % before.length;

      await browser.keyboard.press("ArrowDown");
      await expect
        .poll(async () =>
          (await readResults()).findIndex((result) => result.current),
        )
        .toBe(nextIndex);

      const after = await readResults();
      expect(after[initialIndex]?.current).toBe(false);
      for (const results of [before, after]) {
        const current = results.filter((result) => result.current);
        expect(current).toHaveLength(1);
        expect(current[0]?.foreground).toBe("rgb(255, 255, 255)");
        expect(current[0]?.background).toBe("rgb(59, 91, 253)");
        for (const result of results) {
          expect(result.contrast).toBeGreaterThanOrEqual(4.5);
        }
      }
    });
  }

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

  const heroButtonSelector = ".rp-home-hero__action.rp-button--brand";
  const wordmarkSelector = ".rp-home-hero__title-brand";

  const readHeroButtonContrast = async (fixtures: WebFixtures) => {
    const { stops, label, buttonLabelLowest, ...state } =
      await fixtures.browser.evaluate((selector) => {
        const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
        const button = document.querySelector(selector);
        if (!button) {
          throw new Error("The hero button is missing");
        }

        const style = getComputedStyle(button);
        const label = kit.parse(style.color);
        const stops = kit.stops(style.backgroundImage);
        const backing = stops.some((stop) => stop.a < 1)
          ? kit.parse(style.backgroundColor)
          : null;

        return {
          label,
          stops,
          buttonOpacity: Number(style.opacity),
          buttonTransition: style.transitionProperty,
          buttonShadow: style.boxShadow,
          buttonHovered: button.matches(":hover"),
          buttonAnimating: button.getAnimations().length > 0,
          buttonLabelLowest:
            backing && backing.a !== 1
              ? null
              : Math.min(
                  ...stops.map((stop) =>
                    kit.ratio(label, backing ? kit.over(stop, backing) : stop),
                  ),
                ),
        };
      }, heroButtonSelector);

    return {
      ...state,
      label: [label.r, label.g, label.b, label.a].join(","),
      buttonLabelLowest:
        buttonLabelLowest ??
        (await readHeroBackdrop(
          fixtures,
          "hero-button-backing",
          heroButtonSelector,
          {
            "background-image": "none",
            color: "transparent",
            "-webkit-text-fill-color": "transparent",
            "text-shadow": "none",
          },
          stops,
          label,
        )),
    };
  };

  const readHeroBackdrop = async (
    { app, browser }: WebFixtures,
    screenshotLabel: string,
    selector: string,
    hiddenStyles: Record<string, string>,
    stops: Rgba[],
    label?: Rgba,
  ) => {
    const { originalStyles, ...sample } = await browser.evaluate(
      async ({ selector, hiddenStyles }) => {
        await document.fonts.ready;
        const element = document.querySelector<HTMLElement>(selector);
        if (!element) {
          throw new Error(`Cannot find the hero backdrop target ${selector}`);
        }

        const text = document.createRange();
        text.selectNodeContents(element);
        // The element can be wider than the text it paints.
        const rects = Array.from(
          text.getClientRects(),
          ({ left, top, right, bottom }) => ({ left, top, right, bottom }),
        );
        const originalStyles = Object.entries(hiddenStyles).map(
          ([property, value]) => {
            const original = element.style.getPropertyValue(property);
            const priority = element.style.getPropertyPriority(property);
            element.style.setProperty(property, value, "important");
            return { property, original, priority };
          },
        );
        return {
          rects,
          width: window.innerWidth,
          height: window.innerHeight,
          originalStyles,
        };
      },
      { selector, hiddenStyles },
    );

    let screenshot: string;
    try {
      screenshot = await app.screenshot(screenshotLabel);
    } finally {
      await browser.evaluate(
        ({ selector, originalStyles }) => {
          const element = document.querySelector<HTMLElement>(selector);
          if (!element) {
            throw new Error(
              `Cannot restore the hero backdrop target ${selector}`,
            );
          }
          for (const { property, original, priority } of originalStyles) {
            element.style.setProperty(property, original, priority);
          }
          return true;
        },
        { selector, originalStyles },
      );
    }

    const png = await readFile(screenshot, "base64");
    return browser.evaluate(
      async ({ png, rects, stops, width, height, selector, label }) => {
        const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
        const image = new Image();
        image.src = `data:image/png;base64,${png}`;
        await image.decode();
        const canvas = document.createElement("canvas");
        canvas.width = image.naturalWidth;
        canvas.height = image.naturalHeight;
        const context = canvas.getContext("2d");
        if (!context) {
          throw new Error(
            `Cannot sample the hero backdrop for ${selector}: canvas is unavailable`,
          );
        }
        context.drawImage(image, 0, 0);

        // Convert viewport CSS coordinates to screenshot pixels and clip them
        // to the part of each text rectangle that the browser captured.
        const scaleX = canvas.width / width;
        const scaleY = canvas.height / height;
        let lowest = Infinity;
        for (const rect of rects) {
          const left = Math.max(0, Math.floor(rect.left * scaleX));
          const top = Math.max(0, Math.floor(rect.top * scaleY));
          const right = Math.min(canvas.width, Math.ceil(rect.right * scaleX));
          const bottom = Math.min(
            canvas.height,
            Math.ceil(rect.bottom * scaleY),
          );
          if (right <= left || bottom <= top) {
            continue;
          }
          const { data } = context.getImageData(
            left,
            top,
            right - left,
            bottom - top,
          );
          for (let i = 0; i < data.length; i += 4) {
            if (data[i + 3] !== 255) {
              throw new Error(
                `Cannot sample the hero backdrop for ${selector}: a text backing pixel is translucent`,
              );
            }
            const backdrop = {
              r: data[i]!,
              g: data[i + 1]!,
              b: data[i + 2]!,
              a: 1,
            };
            for (const stop of stops) {
              lowest = Math.min(
                lowest,
                kit.ratio(kit.over(stop, backdrop), label ?? backdrop),
              );
            }
          }
        }
        if (!Number.isFinite(lowest)) {
          throw new Error(
            `Cannot sample the hero backdrop for ${selector}: the screenshot has no visible text pixels`,
          );
        }
        return lowest;
      },
      { png, ...sample, stops, selector, label: label ?? null },
    );
  };

  const readWordmarkContrast = async (
    fixtures: WebFixtures,
    screenshotLabel: string,
  ) => {
    const { stops, wordmarkBackgroundLowest } = await fixtures.browser.evaluate(
      (selector) => {
        const element = document.querySelector(selector);
        if (!element) {
          throw new Error("The hero wordmark is missing");
        }
        const { colorKit: kit } = window as unknown as { colorKit: ColorKit };
        const stops = kit.stops(getComputedStyle(element).backgroundImage);
        const background = kit.resolve("--rp-c-bg");
        if (background.a !== 1) {
          throw new Error("The hero wordmark needs an opaque --rp-c-bg");
        }
        return {
          stops,
          wordmarkBackgroundLowest: Math.min(
            ...stops.map((stop) =>
              kit.ratio(kit.over(stop, background), background),
            ),
          ),
        };
      },
      wordmarkSelector,
    );

    return {
      stops,
      wordmarkBackgroundLowest,
      wordmarkGlowLowest: await readHeroBackdrop(
        fixtures,
        screenshotLabel,
        wordmarkSelector,
        { visibility: "hidden" },
        stops,
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
    await openBrandGuide({ app, browser });
    await setBrandMode(browser, true);

    const dark = await browser.evaluate(readBrandContrast(), true);

    expect(dark.brand).toBe("138,160,255");
    expect(dark.brandLight).toBe("163,180,255");
    expect(dark.brandLighter).toBe("192,204,255");
    expect(dark.contrasts).toHaveLength(12);
    for (const contrast of dark.contrasts) {
      expectDarkBrandContrast(contrast);
    }
  });

  test("keeps the light brand and the hero gradient colors unchanged", async ({
    app,
    browser,
  }) => {
    await openBrandGuide({ app, browser });
    await setBrandMode(browser, false);
    const light = await browser.evaluate(readBrandContrast(), false);
    await setBrandMode(browser, true);
    const dark = await browser.evaluate(readBrandContrast(), false);

    expect(light.brand).toBe("59,91,253");
    expect(dark.brandDark).toBe(light.brandDark);
    expect(dark.brandDarker).toBe(light.brandDarker);
  });

  const overrideBrandProperty = (
    browser: WebFixtures["browser"],
    property: string,
    value: string,
  ) =>
    browser.evaluate(
      ({ property, value }) => {
        document.documentElement.style.setProperty(property, value);
        return true;
      },
      { property, value },
    );

  for (const [token, rgb] of [
    ["--rp-c-brand", "138,160,255"],
    ["--rp-c-brand-light", "163,180,255"],
    ["--rp-c-brand-lighter", "192,204,255"],
  ] as const) {
    test(`rejects insufficient contrast for translucent ${token}`, async ({
      app,
      browser,
    }) => {
      await openBrandGuide({ app, browser });
      await setBrandMode(browser, true);
      await overrideBrandProperty(browser, token, `rgba(${rgb}, 0.01)`);

      const dark = await browser.evaluate(readBrandContrast(), true);

      const affected = dark.contrasts.filter(
        (contrast) => contrast.token === token,
      );
      expect(affected).toHaveLength(4);
      for (const contrast of affected) {
        throws(
          () => expectDarkBrandContrast(contrast),
          new RegExp(`${token} over ${contrast.surface}`),
        );
      }
    });
  }

  for (const [property, value, diagnostic] of [
    ["--rp-c-bg-soft", "initial", /--rp-c-bg-soft is not defined/],
    ["--rp-c-bg-mute", "rgb(52 58 70 / 50%)", /--rp-c-bg-mute must be opaque/],
  ] as const) {
    test(`rejects invalid dark surface ${property}`, async ({
      app,
      browser,
    }) => {
      await openBrandGuide({ app, browser });
      await setBrandMode(browser, true);
      await overrideBrandProperty(browser, property, value);

      await rejects(browser.evaluate(readBrandContrast(), true), diagnostic);
    });
  }

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
      await app.open("/guide/introduction");
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

  for (const mode of ["light", "dark"] as const) {
    test(`keeps the hero button label at 4.5:1 along its gradient in ${mode} mode`, async ({
      app,
      browser,
    }) => {
      await app.open("/");
      await expect(browser.locator(heroButtonSelector)).toBeVisible();
      await expect(browser.locator(".rp-home-hero__title-brand")).toBeVisible();
      await browser.evaluate(installColorKit());
      if (mode === "dark") {
        await browser.evaluate(() => {
          document.documentElement.classList.add("rp-dark");
          return true;
        });
      }

      const hero = await readHeroButtonContrast({ app, browser });

      expect(hero.label).toBe("255,255,255,1");
      expect(hero.buttonLabelLowest).toBeGreaterThanOrEqual(4.5);
    });

    test(`keeps the hero button opaque through hover in ${mode} mode`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/");
      await browser.evaluate(installColorKit());
      await browser.evaluate((dark) => {
        document.documentElement.classList.toggle("rp-dark", dark);
        return true;
      }, mode === "dark");
      const readHero = () => readHeroButtonContrast({ app, browser });
      const action = screen.getByRole("link", "Get started");
      await expect(action).toBeVisible();
      const rest = await readHero();
      expect(rest.buttonHovered).toBe(false);

      await action.hover();
      const entering = await readHero();
      expect(entering.buttonHovered).toBe(true);

      await expect
        .poll(async () => (await readHero()).buttonAnimating)
        .toBe(false);
      const hovered = await readHero();
      expect(hovered.buttonHovered).toBe(true);

      await screen.getByRole("image", "Delino logo").hover();
      const leaving = await readHero();
      expect(leaving.buttonHovered).toBe(false);
      await expect
        .poll(async () => (await readHero()).buttonAnimating)
        .toBe(false);
      const left = await readHero();
      expect(left.buttonHovered).toBe(false);

      for (const state of [rest, entering, hovered, leaving, left]) {
        expect(state.label).toBe("255,255,255,1");
        expect(state.buttonOpacity).toBe(1);
        expect(state.buttonLabelLowest).toBeGreaterThanOrEqual(4.5);
        expect(state.buttonTransition).toBe("box-shadow");
      }
      expect(hovered.buttonShadow).not.toBe(rest.buttonShadow);
      expect(left.buttonShadow).toBe(rest.buttonShadow);
    });

    test(`checks the hero wordmark gradient against the painted backdrop in ${mode} mode`, async ({
      app,
      browser,
    }) => {
      await app.open("/");
      await expect(browser.locator(wordmarkSelector)).toBeVisible();
      await browser.evaluate(installColorKit());
      await browser.evaluate((dark) => {
        document.documentElement.classList.toggle("rp-dark", dark);
        return true;
      }, mode === "dark");

      const stock = await readWordmarkContrast(
        { app, browser },
        `wordmark-${mode}-stock`,
      );
      expect(stock.wordmarkBackgroundLowest).toBeGreaterThanOrEqual(3);
      expect(stock.wordmarkGlowLowest).toBeGreaterThanOrEqual(3);

      const glowProperty = "--rp-home-background-bg";
      const originalGlow = await browser.evaluate(
        ({ property, color }) => {
          const { style } = document.documentElement;
          const value = style.getPropertyValue(property);
          const priority = style.getPropertyPriority(property);
          style.setProperty(property, color);
          return { value, priority };
        },
        {
          property: glowProperty,
          color: mode === "dark" ? "#8aa0ff" : "#5b68e6",
        },
      );
      try {
        const changed = await readWordmarkContrast(
          { app, browser },
          `wordmark-${mode}-overridden`,
        );
        expect(changed.stops).toEqual(stock.stops);
        expect(changed.wordmarkBackgroundLowest).toBe(
          stock.wordmarkBackgroundLowest,
        );
        expect(changed.wordmarkGlowLowest).toBeLessThan(3);
      } finally {
        await browser.evaluate(
          ({ property, value, priority }) => {
            document.documentElement.style.setProperty(
              property,
              value,
              priority,
            );
            return true;
          },
          { property: glowProperty, ...originalGlow },
        );
      }
      await expect(browser.locator(wordmarkSelector)).toBeVisible();
    });

    test(`accounts for translucent hero gradient stops in ${mode} mode`, async ({
      app,
      browser,
    }) => {
      await app.open("/");
      await expect(browser.locator(heroButtonSelector)).toBeVisible();
      await expect(browser.locator(wordmarkSelector)).toBeVisible();
      await browser.evaluate(installColorKit());
      const fixtureId = "hero-contrast-fixture";
      await browser.evaluate(
        ({ fixtureId, dark }) => {
          document.documentElement.classList.toggle("rp-dark", dark);
          const style = document.createElement("style");
          style.id = fixtureId;
          document.head.append(style);
          return true;
        },
        { fixtureId, dark: mode === "dark" },
      );

      const setGradient = (alpha: number, buttonBacking: string) =>
        browser.evaluate(
          ({
            fixtureId,
            alpha,
            buttonBacking,
            heroButtonSelector,
            wordmarkSelector,
          }) => {
            document.getElementById(fixtureId)!.textContent = `
              :root { --rp-c-bg: white !important; }
              .rp-home-hero__container { background-color: rgb(231, 232, 255); }
              ${heroButtonSelector}, ${wordmarkSelector} {
                background-image: linear-gradient(rgb(44, 71, 216), rgba(91, 104, 230, ${alpha})) !important;
              }
              ${heroButtonSelector} {
                background-color: ${buttonBacking} !important;
                opacity: 1 !important;
              }
            `;
            return true;
          },
          {
            fixtureId,
            alpha,
            buttonBacking,
            heroButtonSelector,
            wordmarkSelector,
          },
        );

      try {
        await setGradient(1, "rgb(231, 232, 255)");
        const opaqueButton = await readHeroButtonContrast({ app, browser });
        const opaqueWordmark = await readWordmarkContrast(
          { app, browser },
          `wordmark-${mode}-opaque-control`,
        );
        expect(opaqueButton.buttonLabelLowest).toBeGreaterThanOrEqual(4.5);
        expect(opaqueWordmark.wordmarkBackgroundLowest).toBeGreaterThanOrEqual(
          3,
        );
        expect(opaqueWordmark.wordmarkGlowLowest).toBeGreaterThanOrEqual(3);

        await setGradient(0.01, "rgb(231, 232, 255)");
        const translucentButton = await readHeroButtonContrast({
          app,
          browser,
        });
        const translucentWordmark = await readWordmarkContrast(
          { app, browser },
          `wordmark-${mode}-translucent-control`,
        );
        expect(translucentButton.buttonOpacity).toBe(1);
        expect(translucentButton.buttonLabelLowest).toBeLessThan(4.5);
        expect(translucentWordmark.wordmarkBackgroundLowest).toBeLessThan(3);
        expect(translucentWordmark.wordmarkGlowLowest).toBeLessThan(3);

        await setGradient(0.01, "transparent");
        const sampledButton = await readHeroButtonContrast({ app, browser });
        expect(sampledButton.buttonOpacity).toBe(1);
        expect(sampledButton.buttonLabelLowest).toBe(
          translucentButton.buttonLabelLowest,
        );
      } finally {
        await browser.evaluate((id) => {
          document.getElementById(id)?.remove();
          return true;
        }, fixtureId);
      }
    });
  }
});
