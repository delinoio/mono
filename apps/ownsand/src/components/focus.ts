// SPDX-License-Identifier: Apache-2.0

/**
 * Focus fallback for a dialog whose opener is gone. A controlled dialog has
 * no trigger, so after a confirmed action removes its opener (a removed
 * member row, a revoked key, a billing section that changes its buttons) or
 * after the console navigates to another page, the UI SDK has no element to
 * return focus to and the keyboard focus falls back to the page body. A
 * keyboard or screen reader user then starts again from the top of the
 * document without knowing where they are.
 *
 * `focusPageHeading` moves the focus to the page heading (`main h1`, which
 * has `tabindex="-1"`, so it is a focus target and not a Tab stop) only while
 * nothing holds the focus. An opener that still exists got the focus back
 * from the SDK, and a field or button that the user focused keeps it.
 */
export function focusPageHeading(): void {
  if (isHoldingFocus(document.activeElement)) return;
  // The heading is a landmark for the keyboard position, not a scroll target.
  document
    .querySelector<HTMLElement>("main h1")
    ?.focus({ preventScroll: true });
}

/**
 * Whether `active` is something the user is working with. Nothing is, when
 * the page body holds the focus, and neither is an element inside a dialog
 * that has finished closing: the UI SDK removes `data-open` from a dialog
 * when it closes, and a browser that has not yet moved the focus out of the
 * hidden dialog (happy-dom never does) still reports its button as active.
 */
function isHoldingFocus(active: Element | null): boolean {
  if (active === null || active === document.body) return false;
  const popup = active.closest('[role="dialog"], [role="alertdialog"]');
  return popup === null || popup.hasAttribute("data-open");
}

/** Whether a navigation still waits for its page heading to render. */
let headingFocusPending = false;

/**
 * Moves the focus to the heading of the page that an in-console navigation
 * opened, when nothing holds the focus. A page that renders its heading only
 * after its data arrived (for example a new organization's pages) is focused
 * when its heading mounts, through `focusPendingPageHeading`.
 */
export function focusPageHeadingAfterNavigation(): void {
  // Wait only while the page has no heading; a page that has one either gets
  // the focus now or keeps what holds it.
  headingFocusPending = document.querySelector("main h1") === null;
  focusPageHeading();
}

/** Called when a page heading mounts; completes a pending navigation focus. */
export function focusPendingPageHeading(): void {
  if (!headingFocusPending) return;
  headingFocusPending = false;
  focusPageHeading();
}

/**
 * The `onOpenChangeComplete` handler of the dialogs: when a dialog has
 * finished closing, fall back to the page heading if nothing holds the focus.
 */
export function focusPageHeadingAfterClose(open: boolean): void {
  if (!open) focusPageHeading();
}
