// SPDX-License-Identifier: Apache-2.0
import { type ComponentProps, useEffect, useId, useRef, useState } from "react";
import { useI18n } from "@rspress/core/runtime";
import {
  IconSmallMenu,
  SocialLinks,
  SvgWrapper,
  type NavHamburger as BaseNavHamburger,
} from "@rspress/core/theme-original";
import { NavVersions } from "@rspress/core/dist/theme/components/Nav/NavMenu.js";
import { useNavScreen } from "@rspress/core/dist/theme/components/NavHamburger/useNavScreen.js";
import {
  NavScreen,
  NavScreenDivider,
} from "@rspress/core/dist/theme/components/NavScreen/index.js";
import { NavScreenAppearance } from "@rspress/core/dist/theme/components/NavScreen/NavScreenAppearance.js";
import { NavScreenLangs } from "@rspress/core/dist/theme/components/NavScreen/NavScreenLangs.js";
import "@rspress/core/dist/theme/components/NavHamburger/index.css";
import "@rspress/core/dist/theme/components/HoverGroup/index.css";
import "./compact-nav.css";

// These paths use the pinned Rspress 2.1.3 exports. Remove the site override
// when upstream puts the medium popup after its trigger in keyboard order.
export function NavHamburger(props: ComponentProps<typeof BaseNavHamburger>) {
  const t = useI18n();
  const screenId = useId();
  const trigger = useRef<HTMLButtonElement>(null);
  const { isScreenOpen, toggleScreen, closeScreen } = useNavScreen();

  return (
    <>
      <div
        className="site-nav-screen"
        onBlur={(event) => {
          if (
            event.relatedTarget &&
            !event.currentTarget.contains(event.relatedTarget)
          ) {
            closeScreen();
          }
        }}
        onKeyDown={(event) => {
          if (event.key === "Escape" && isScreenOpen) {
            event.preventDefault();
            closeScreen();
            trigger.current?.focus();
          }
        }}
      >
        <button
          ref={trigger}
          type="button"
          aria-label={t("menuTitle")}
          aria-expanded={isScreenOpen}
          aria-controls={isScreenOpen ? screenId : undefined}
          className={`rp-nav-hamburger rp-nav-hamburger__sm${isScreenOpen ? " rp-nav-hamburger--active" : ""}`}
          onClick={toggleScreen}
        >
          <SvgWrapper
            aria-hidden="true"
            icon={IconSmallMenu}
            width={20}
            height={20}
          />
        </button>
        {isScreenOpen && (
          <div id={screenId}>
            <NavScreen
              {...props}
              isScreenOpen={isScreenOpen}
              toggleScreen={toggleScreen}
            />
          </div>
        )}
      </div>
      <MediumNav />
    </>
  );
}

function MediumNav() {
  const t = useI18n();
  const popupId = useId();
  const disclosure = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (!open) return;

    function dismissOutside(event: PointerEvent) {
      if (
        event.target instanceof Node &&
        !disclosure.current?.contains(event.target)
      ) {
        setOpen(false);
      }
    }

    document.addEventListener("pointerdown", dismissOutside);
    return () => document.removeEventListener("pointerdown", dismissOutside);
  }, [open]);

  return (
    <div
      ref={disclosure}
      className="site-nav-popover"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={(event) => {
        if (!event.currentTarget.contains(document.activeElement)) {
          setOpen(false);
        }
      }}
      onBlur={(event) => {
        if (
          event.relatedTarget &&
          !event.currentTarget.contains(event.relatedTarget)
        ) {
          setOpen(false);
        }
      }}
      onKeyDown={(event) => {
        if (event.key === "Escape" && open) {
          event.preventDefault();
          setOpen(false);
          trigger.current?.focus();
        }
      }}
    >
      <button
        ref={trigger}
        type="button"
        aria-label={t("menuTitle")}
        aria-expanded={open}
        aria-controls={popupId}
        className="rp-nav-hamburger rp-nav-hamburger__md"
        onClick={(event) => {
          // A mouse click can follow hover opening. Keyboard activation toggles.
          setOpen(event.detail === 0 ? !open : true);
        }}
      >
        <SvgWrapper aria-hidden="true" icon={IconSmallMenu} />
      </button>
      <div
        id={popupId}
        inert={!open}
        className={`rp-hover-group rp-hover-group--right${open ? "" : " rp-hover-group--hidden"}`}
        onClick={(event) => {
          if (
            event.target instanceof Element &&
            event.target.closest("a[href]")
          ) {
            setOpen(false);
          }
        }}
      >
        <div className="rp-nav-hamburger__md__hover-group">
          <NavScreenAppearance />
          <NavVersions />
          <NavScreenLangs />
          <NavScreenDivider />
          <SocialLinks />
        </div>
      </div>
    </div>
  );
}
