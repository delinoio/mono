// SPDX-License-Identifier: Apache-2.0

/** Production origin of ownsand-api. */
export const DEFAULT_API_ORIGIN = "https://ownsand.api.delino.io";

/**
 * Logto API resource indicator of ownsand-api. It names the API in the shared
 * Logto tenant and stays the same when `PUBLIC_API_ORIGIN` points to a local
 * server, because the server validates this audience.
 */
export const API_RESOURCE = "https://ownsand.api.delino.io";

/** Account settings live in delino-accounts; the console has no profile pages. */
export const ACCOUNT_SETTINGS_URL = "https://accounts.delino.io";

/**
 * Matches the leading and trailing run of C0 control characters (U+0000 to
 * U+001F) and spaces that the URL parser removes before it parses a value.
 */
// The pattern must name control characters to mirror the URL parser.
/* eslint-disable no-control-regex */
const PARSER_EDGE_STRIP = new RegExp(
  "^[\\u0000-\\u0020]+|[\\u0000-\\u0020]+$",
  "g",
);
/* eslint-enable no-control-regex */

/** Build-time environment of the console. Every value is public. */
export interface ConsoleBuildEnv {
  readonly PUBLIC_API_ORIGIN?: string;
  readonly PUBLIC_LOGTO_ENDPOINT?: string;
  readonly PUBLIC_LOGTO_APP_ID?: string;
  readonly PUBLIC_OWNSAND_E2E_FAKE_AUTH?: string;
}

/** Logto settings of the console's single-page application registration. */
export interface LogtoSettings {
  readonly endpoint: string;
  readonly appId: string;
}

/** Validated build-time configuration. */
export interface ConsoleConfig {
  readonly apiOrigin: string;
  /** `undefined` when sign-in is not configured for this build. */
  readonly logto: LogtoSettings | undefined;
  /** True only for development servers started by the e2e suite. */
  readonly e2eFakeAuth: boolean;
}

/** Name of the build-time variable that enables the e2e sign-in stand-in. */
export const E2E_FAKE_AUTH_VARIABLE = "PUBLIC_OWNSAND_E2E_FAKE_AUTH";

/**
 * Reads and validates the build-time configuration. `rsbuild.config.ts`
 * passes `process.env` to fail the build early, and `src/env.ts` passes
 * `import.meta.env` for the client bundle.
 */
export function readConsoleConfig(env: ConsoleBuildEnv): ConsoleConfig {
  return {
    apiOrigin: resolveOrigin(
      "PUBLIC_API_ORIGIN",
      env.PUBLIC_API_ORIGIN,
      DEFAULT_API_ORIGIN,
    ),
    logto: readLogtoSettings(env),
    e2eFakeAuth: readE2eFakeAuth(env.PUBLIC_OWNSAND_E2E_FAKE_AUTH),
  };
}

function isBlank(value: string | undefined): value is undefined {
  // Only ASCII white space counts as empty; see resolveOrigin.
  return value === undefined || /^[\t-\r ]*$/.test(value);
}

function readLogtoSettings(env: ConsoleBuildEnv): LogtoSettings | undefined {
  const endpointBlank = isBlank(env.PUBLIC_LOGTO_ENDPOINT);
  const appIdBlank = isBlank(env.PUBLIC_LOGTO_APP_ID);
  if (endpointBlank && appIdBlank) {
    return undefined;
  }
  if (endpointBlank || appIdBlank) {
    throw new Error(
      "PUBLIC_LOGTO_ENDPOINT and PUBLIC_LOGTO_APP_ID must be set together",
    );
  }
  const appId = env.PUBLIC_LOGTO_APP_ID.trim();
  if (!/^[A-Za-z0-9_-]{1,128}$/.test(appId)) {
    throw new Error(
      "PUBLIC_LOGTO_APP_ID must contain only letters, digits, '-' and '_'",
    );
  }
  return {
    endpoint: resolveOrigin(
      "PUBLIC_LOGTO_ENDPOINT",
      env.PUBLIC_LOGTO_ENDPOINT,
      undefined,
    ),
    appId,
  };
}

function readE2eFakeAuth(value: string | undefined): boolean {
  if (isBlank(value)) {
    return false;
  }
  if (value === "1") {
    return true;
  }
  throw new Error(`${E2E_FAKE_AUTH_VARIABLE} must be unset or "1"`);
}

/**
 * Resolves an origin from a build-time value. An unset or empty value selects
 * `fallback`; without a fallback the value is required. Values with
 * credentials, a path, a query or a fragment are rejected so that RPC paths
 * are never resolved against a wrong base and credentials never reach the
 * public bundle.
 *
 * Every `PUBLIC_` variable is inlined into the client bundle and build logs
 * are often public, so no error message echoes the raw value: it may carry a
 * credential, including inside an otherwise malformed value.
 *
 * The rules match the `delino-accounts` resolver
 * (`apps/delino-accounts/src/config.ts`); apps do not import each other.
 */
export function resolveOrigin(
  name: string,
  value: string | undefined,
  fallback: string | undefined,
): string {
  // Do not use trim(): it also removes Unicode spaces (for example U+00A0,
  // U+FEFF and U+3000) that the URL parser keeps and rejects.
  let raw: string;
  if (isBlank(value)) {
    if (fallback === undefined) {
      throw new Error(`${name} is required`);
    }
    raw = fallback;
  } else {
    raw = value;
  }
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new Error(`${name} is not a valid URL`);
  }
  // The credential check runs first so that no later diagnostic can leak it.
  if (url.username !== "" || url.password !== "") {
    throw new Error(`${name} must not contain credentials`);
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    // Echo the protocol only when the raw value really starts with a scheme;
    // otherwise the "protocol" can be the user part of a credential.
    if (/^[A-Za-z][A-Za-z0-9+.-]*:\/\//.test(raw)) {
      throw new Error(`${name} must use http or https, got ${url.protocol}`);
    }
    throw new Error(`${name} must use http or https`);
  }
  // `url.pathname` is normalized (`/..` collapses to `/`), and `url.search` and
  // `url.hash` are empty for an empty `?` or `#`, so the raw text after the
  // authority is checked too, after the same edge stripping as the parser.
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
      `${name} must be an origin without a path, query or fragment`,
    );
  }
  return url.origin;
}
