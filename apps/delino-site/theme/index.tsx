// SPDX-License-Identifier: Apache-2.0
// Keep the site stylesheets first, as before the `Link` override, so the
// order of the theme and Rspress styles does not change.
import "./index.css";
import "./nav-focus.css";
import { forwardRef, type ComponentProps } from "react";
import { useLang, usePage } from "@rspress/core/runtime";
import {
  Link as BaseLink,
  LlmsHint as BaseLlmsHint,
} from "@rspress/core/theme-original";
import { localizeCrossSiteHref } from "./cross-site-link";

export * from "@rspress/core/theme-original";
export { NavHamburger } from "./nav-hamburger";
export { SwitchAppearance } from "./switch-appearance";

// Rspress emits the synthetic 404 page only as HTML, but its default hint
// advertises a Markdown alternate for every route. Remove this override when
// the default hint stops advertising Markdown for synthetic 404 pages.
export function LlmsHint() {
  const { page } = usePage();
  return page?.pageType === "404" ? null : <BaseLlmsHint />;
}

// Workaround: the marketing site has no documentation search (docs.delino.io
// owns it), but the Rspress 2.1.3 default theme `Nav` mounts `<Search />`
// unconditionally and ignores `themeConfig.search`. Exporting `Search` here
// takes precedence over the re-exported theme-original `Search`, so the nav
// renders no search button and no Ctrl/Cmd+K handler is registered.
// Scope: delino-site only; delino-docs keeps the built-in search.
// Remove when the pinned `@rspress/core` honours `themeConfig.search: false`
// in the default `Nav`.
export function Search() {
  return null;
}

// Links to Delino Docs keep the language of the current page. The default
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
