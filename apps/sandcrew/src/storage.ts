// SPDX-License-Identifier: Apache-2.0

/**
 * Per-browser conveniences in `localStorage`: the selected organization and
 * the session list filter. Every access is wrapped, because storage can be
 * blocked or full; the console works without it.
 */

const SELECTED_ORGANIZATION_KEY = "sandcrew.selected-organization";
const SESSION_FILTER_KEY = "sandcrew.session-filter";

/** The session list filter. */
export enum SessionFilter {
  Mine = "mine",
  All = "all",
}

function read(key: string): string | undefined {
  try {
    return window.localStorage.getItem(key) ?? undefined;
  } catch {
    return undefined;
  }
}

function write(key: string, value: string | undefined): void {
  try {
    if (value === undefined) {
      window.localStorage.removeItem(key);
    } else {
      window.localStorage.setItem(key, value);
    }
  } catch {
    // Storage is optional.
  }
}

export function readSelectedOrganization(): string | undefined {
  return read(SELECTED_ORGANIZATION_KEY);
}

export function writeSelectedOrganization(organizationId: string | undefined) {
  write(SELECTED_ORGANIZATION_KEY, organizationId);
}

/** The stored filter, "My sessions" by default. */
export function readSessionFilter(): SessionFilter {
  return read(SESSION_FILTER_KEY) === SessionFilter.All
    ? SessionFilter.All
    : SessionFilter.Mine;
}

export function writeSessionFilter(filter: SessionFilter): void {
  write(SESSION_FILTER_KEY, filter);
}
