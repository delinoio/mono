// SPDX-License-Identifier: Apache-2.0

/** Production origin of accounts-api. */
export const DEFAULT_ACCOUNTS_API_ORIGIN = "https://accounts.api.delino.io";

/**
 * Matches the leading and trailing run of C0 control characters (U+0000 to
 * U+001F) and spaces that the URL parser removes before it parses a value.
 */
const PARSER_EDGE_STRIP = new RegExp(
  "^[\\u0000-\\u0020]+|[\\u0000-\\u0020]+$",
  "g",
);

/** Build-time environment that carries `PUBLIC_API_ORIGIN`. */
export interface AccountsBuildEnv {
  readonly PUBLIC_API_ORIGIN?: string;
}

/**
 * Reads and validates the API server origin from a build-time environment.
 * This is the only place that reads `PUBLIC_API_ORIGIN`: the Rsbuild config
 * passes `process.env` and the client bundle passes `import.meta.env`.
 */
export function readAccountsApiOrigin(env: AccountsBuildEnv): string {
  return resolveAccountsApiOrigin(env.PUBLIC_API_ORIGIN);
}

/**
 * Resolves the API server origin from a build-time value. An unset or empty
 * value selects the production origin. Values with credentials, a path, a
 * query or a fragment are rejected so that RPC paths are never resolved
 * against a wrong base and credentials never reach the public bundle.
 *
 * Every `PUBLIC_` variable is inlined into the client bundle and build logs
 * are often public, so no error message echoes the raw value: it may carry a
 * credential, including inside an otherwise malformed value.
 */
export function resolveAccountsApiOrigin(value: string | undefined): string {
  // Do not use trim(): it also removes Unicode spaces (for example U+00A0,
  // U+FEFF and U+3000) that the URL parser keeps and rejects. Only a value
  // made of tab, line feed, vertical tab, form feed, carriage return and space
  // counts as empty. The original value goes to the parser unchanged.
  const raw =
    value === undefined || /^[\t-\r ]*$/.test(value)
      ? DEFAULT_ACCOUNTS_API_ORIGIN
      : value;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new Error("PUBLIC_API_ORIGIN is not a valid URL");
  }
  // The credential check runs first so that no later diagnostic can leak it.
  // `url.origin` silently drops userinfo, so it has to be checked explicitly.
  if (url.username !== "" || url.password !== "") {
    throw new Error("PUBLIC_API_ORIGIN must not contain credentials");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    // The scheme text can itself be a credential (`sk-proj-abc123://host`,
    // `token123:secret@host`), so the message never echoes it.
    throw new Error("PUBLIC_API_ORIGIN must use http or https");
  }
  // `url.search` and `url.hash` are empty both when a component is absent and
  // when it is present but empty (`https://host?`, `https://host#`), so the
  // raw value is checked for the delimiters. A valid origin never contains them.
  //
  // `url.pathname` is normalized, so `/..`, `/.`, `/%2e` and `/a/..` collapse to
  // `/` and a backslash acts as a slash. The raw text after the authority is
  // therefore checked too. The URL parser removes leading and trailing C0
  // control characters and spaces, and drops tab, CR and LF anywhere; the
  // scheme is followed by any run of `/` or `\` (`https:host` and
  // `https:///host` are accepted by the parser), which is stripped together
  // with the authority. The control characters are matched by code point range
  // through `RegExp` so the pattern mirrors the parser without a control
  // character in a regular expression literal.
  const rawPath = raw
    .replace(PARSER_EDGE_STRIP, "")
    .replace(/[\t\n\r]/g, "")
    .replace(/^[A-Za-z][A-Za-z0-9+.-]*:[/\\]*/, "")
    .replace(/^[^/\\?#]*/, "");
  if (
    url.pathname !== "/" ||
    url.search !== "" ||
    url.hash !== "" ||
    raw.includes("?") ||
    raw.includes("#") ||
    (rawPath !== "" && rawPath !== "/")
  ) {
    throw new Error(
      "PUBLIC_API_ORIGIN must be an origin without a path, query or fragment",
    );
  }
  return url.origin;
}
