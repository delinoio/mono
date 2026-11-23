// SPDX-License-Identifier: Apache-2.0

/**
 * The rows of a labelled list, read from the DOM: the `li` children of the
 * `ul` with the `aria-label` `label`, in document order.
 *
 * Tests use this instead of `getAllByRole("listitem")` for the lists of 200
 * rows that the paging tests build. A role query computes the accessible role
 * of every element of the list, which costs tens of milliseconds in the test
 * DOM, and `waitFor` and `findBy*` repeat the query on every DOM mutation
 * while the rows render. On a loaded CI runner that exceeded the 5 s test
 * timeout. The elements are the same ones that the role query returns for
 * these lists.
 */
export function listRows(label: string): HTMLElement[] {
  const list = document.querySelector<HTMLElement>(`ul[aria-label="${label}"]`);
  if (list === null) throw new Error(`no list labelled ${label}`);
  return listItems(list);
}

/** The `li` children of `list`, in document order. */
export function listItems(list: HTMLElement): HTMLElement[] {
  return Array.from(list.querySelectorAll<HTMLElement>(":scope > li"));
}
