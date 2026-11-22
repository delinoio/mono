// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

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
});
