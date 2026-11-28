// SPDX-License-Identifier: Apache-2.0
import { useContext, useSyncExternalStore, type ComponentProps } from "react";
import { ThemeContext, useI18n } from "@rspress/core/runtime";
import {
  IconMoon,
  IconSun,
  SvgWrapper,
  type SwitchAppearance as BaseSwitchAppearance,
} from "@rspress/core/theme-original";
import "@rspress/core/dist/theme/components/SwitchAppearance/index.css";

const emptySubscribe = () => () => {};

// Rspress 2.1.3 uses a click-only div. This site override keeps its icons
// and theme state. Remove it when the upstream switch is a named toggle
// button and the compact navigation can render it outside its trigger.
export function SwitchAppearance({
  onClick,
}: ComponentProps<typeof BaseSwitchAppearance>) {
  const { theme, setTheme } = useContext(ThemeContext);
  const t = useI18n<{ darkModeLabel: string }>();
  const dark = theme === "dark";
  // The static HTML uses light mode. Match it during hydration so React
  // updates the pressed state when the browser resolves the current theme.
  const pressed = useSyncExternalStore(
    emptySubscribe,
    () => dark,
    () => false,
  );

  return (
    <button
      type="button"
      className="rp-switch-appearance"
      aria-label={t("darkModeLabel")}
      aria-pressed={pressed}
      onClick={() => {
        // The site uses the Rspress default with appearance animation off.
        setTheme?.(dark ? "light" : "dark");
        onClick?.();
      }}
    >
      <SvgWrapper
        aria-hidden="true"
        className="rp-switch-appearance__icon rp-switch-appearance__icon--sun"
        icon={IconSun}
        fill="currentColor"
      />
      <SvgWrapper
        aria-hidden="true"
        className="rp-switch-appearance__icon rp-switch-appearance__icon--moon"
        icon={IconMoon}
        fill="currentColor"
      />
    </button>
  );
}
