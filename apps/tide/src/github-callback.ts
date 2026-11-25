// SPDX-License-Identifier: Apache-2.0

/**
 * The GitHub App setup callback: GitHub returns to `/github/setup` with
 * `installation_id`, `code` and `state` in the query. The console keeps them
 * only in memory until it sends them to `LinkGitHubInstallation`, and
 * removes them from the address bar at once.
 */

/** The query parameters of the callback. */
export enum GitHubCallbackParam {
  InstallationId = "installation_id",
  Code = "code",
  State = "state",
  SetupAction = "setup_action",
}

export interface GitHubCallback {
  githubInstallationId: bigint;
  code: string;
  state: string;
}

const MAX_INT64 = 2n ** 63n - 1n;

/** Parses the callback query; undefined when a value is missing or invalid. */
export function parseGitHubCallback(
  search: string,
): GitHubCallback | undefined {
  const params = new URLSearchParams(search);
  const id = params.get(GitHubCallbackParam.InstallationId) ?? "";
  const code = params.get(GitHubCallbackParam.Code) ?? "";
  const state = params.get(GitHubCallbackParam.State) ?? "";
  if (!/^[1-9][0-9]{0,18}$/.test(id) || code === "" || state === "") {
    return undefined;
  }
  const githubInstallationId = BigInt(id);
  if (githubInstallationId > MAX_INT64) {
    return undefined;
  }
  return { githubInstallationId, code, state };
}

/**
 * Removes the callback parameters (and every other query parameter) from
 * the address bar without a navigation.
 */
export function stripCallbackFromAddress(
  location: Pick<Location, "pathname" | "search" | "hash">,
  history: Pick<History, "replaceState">,
): void {
  if (location.search !== "") {
    history.replaceState(null, "", location.pathname + location.hash);
  }
}
