// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { cleanup, render } from "@testing-library/react";
import { Markdown } from "./markdown.tsx";

afterEach(() => {
  cleanup();
});

describe("Markdown", () => {
  test("renders GitHub Flavored Markdown", () => {
    const { container } = render(
      <Markdown
        text={[
          "# Result",
          "",
          "Use **bold**, `code` and ~~old~~.",
          "",
          "| File | Status |",
          "| --- | --- |",
          "| a.go | changed |",
          "",
          "- [x] tests",
          "",
          "```go",
          "func main() {}",
          "```",
        ].join("\n")}
      />,
    );
    expect(container.querySelector("h1, h2")).toBeNull();
    expect(container.querySelector("h3")?.textContent).toBe("Result");
    expect(container.querySelector("strong")?.textContent).toBe("bold");
    expect(container.querySelector("del")?.textContent).toBe("old");
    expect(container.querySelector("table td")?.textContent).toBe("a.go");
    expect(
      container.querySelector<HTMLInputElement>("input[type=checkbox]")
        ?.checked,
    ).toBe(true);
    expect(container.querySelector("pre.code-block code")?.textContent).toBe(
      "func main() {}\n",
    );
  });

  test("does not render raw HTML", () => {
    const { container } = render(
      <Markdown
        text={'<img src="x" onerror="alert(1)"> <script>alert(1)</script> text'}
      />,
    );
    expect(container.querySelector("img, script")).toBeNull();
    expect(container.textContent).toContain("text");
  });

  test("keeps only safe link URLs and opens links in a new tab", () => {
    const { container } = render(
      <Markdown
        text={
          "[docs](https://example.com/docs) [bad](javascript:alert(1)) ![img](data:image/png;base64,AA)"
        }
      />,
    );
    const links = [...container.querySelectorAll("a")];
    expect(links[0]?.getAttribute("href")).toBe("https://example.com/docs");
    expect(links[0]?.getAttribute("rel")).toBe("noopener noreferrer");
    expect(links[0]?.getAttribute("target")).toBe("_blank");
    expect(links[1]?.hasAttribute("href")).toBe(false);
    expect(container.querySelector("img")?.hasAttribute("src")).toBe(false);
  });
});
