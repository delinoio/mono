// SPDX-License-Identifier: Apache-2.0

/**
 * Leaves the console for another site, for example the GitHub App
 * installation page. Tests replace `assign` to observe the navigation.
 */
export const externalNavigation = {
  assign(url: string): void {
    window.location.assign(url);
  },
};
