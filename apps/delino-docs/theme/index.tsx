// SPDX-License-Identifier: Apache-2.0
// Keep the brand stylesheet first, as before the `Link` override, so the
// order of the theme and Rspress styles does not change.
import "./index.css";
import { forwardRef, type ComponentProps } from "react";
import { useLang } from "@rspress/core/runtime";
import { Link as BaseLink } from "@rspress/core/theme-original";
import { localizeCrossSiteHref } from "./cross-site-link";

export * from "@rspress/core/theme-original";

// Links to Delino Site keep the language of the current page. The default
// theme renders navigation items, home hero actions and Markdown links
// through this export, so it covers the links of translated pages, whose
// frontmatter keeps the English URL unchanged.
export const Link = forwardRef<
  HTMLAnchorElement,
  ComponentProps<typeof BaseLink>
>(({ href, ...props }, ref) => {
  const lang = useLang();
  return (
    <BaseLink
      {...props}
      href={href === undefined ? undefined : localizeCrossSiteHref(href, lang)}
      ref={ref}
    />
  );
});
Link.displayName = "Link";
