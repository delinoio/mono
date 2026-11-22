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

/** The Logto API resource indicator of accounts-api. */
export const API_RESOURCE = "https://accounts.api.delino.io";

/** The API resource scope that every user RPC of accounts-api requires. */
export const REQUIRED_API_SCOPE = "accounts:billing";

/** Build-time environment that carries `PUBLIC_API_ORIGIN`. */
export interface AccountsBuildEnv {
  readonly PUBLIC_API_ORIGIN?: string;
}

/** Every build-time variable of the web app. All are public. */
export interface AccountsAppBuildEnv extends AccountsBuildEnv {
  readonly PUBLIC_LOGTO_ENDPOINT?: string;
  readonly PUBLIC_LOGTO_APP_ID?: string;
  readonly PUBLIC_PADDLE_CLIENT_TOKEN?: string;
  readonly PUBLIC_PADDLE_ENVIRONMENT?: string;
}

/** Paddle environments that `PUBLIC_PADDLE_ENVIRONMENT` selects. */
export enum PaddleEnvironment {
  Sandbox = "sandbox",
  Production = "production",
}

export interface LogtoSettings {
  endpoint: string;
  appId: string;
  scopes: string[];
}

export interface PaddleSettings {
  /** The Paddle client-side token. Public, not a secret. */
  clientToken: string;
  environment: PaddleEnvironment;
}

export interface AccountsConfig {
  apiOrigin: string;
  /** Undefined when the Logto registration is not configured. */
  logto: LogtoSettings | undefined;
  /** Undefined when the Paddle checkout is not configured. */
  paddle: PaddleSettings | undefined;
}

/**
 * Reads and validates the whole web app configuration. `rsbuild.config.ts`
 * calls it with `process.env` to fail the build early, and
 * `src/build-config.ts` calls it with `import.meta.env`.
 *
 * - Without both `PUBLIC_LOGTO_ENDPOINT` and `PUBLIC_LOGTO_APP_ID`, sign-in
 *   is not configured and the billing pages show a notice.
 * - Without `PUBLIC_PADDLE_CLIENT_TOKEN`, the subscription checkout is not
 *   configured. With a token, `PUBLIC_PADDLE_ENVIRONMENT` is required and
 *   must match the token: Paddle sandbox tokens start with `test_` and live
 *   tokens with `live_`.
 *
 * Error messages never echo the raw values.
 */
export function readAccountsConfig(env: AccountsAppBuildEnv): AccountsConfig {
  const apiOrigin = readAccountsApiOrigin(env);
  const endpoint = (env.PUBLIC_LOGTO_ENDPOINT ?? "").trim();
  const appId = (env.PUBLIC_LOGTO_APP_ID ?? "").trim();
  if ((endpoint === "") !== (appId === "")) {
    throw new Error(
      "PUBLIC_LOGTO_ENDPOINT and PUBLIC_LOGTO_APP_ID must be set together",
    );
  }
  const logto =
    endpoint === ""
      ? undefined
      : {
          endpoint: resolveLogtoEndpoint(endpoint),
          appId,
          scopes: [REQUIRED_API_SCOPE],
        };
  return { apiOrigin, logto, paddle: readPaddleSettings(env) };
}

/**
 * Returns the raw text that follows the authority of a URL string, before the
 * URL parser normalizes it. `url.pathname` collapses `/..`, `/.`, `/%2e` and
 * `/a/..` to `/` and treats a backslash as a slash, so a validator that must
 * accept only a bare origin checks this text as well.
 *
 * The URL parser removes leading and trailing C0 control characters and
 * spaces, and drops tab, CR and LF anywhere; the scheme is followed by any run
 * of `/` or `\` (`https:host` and `https:///host` are accepted by the parser),
 * which is stripped together with the authority. The control characters are
 * matched by code point range through `RegExp` so the pattern mirrors the
 * parser without a control character in a regular expression literal.
 */
function rawPathAfterAuthority(raw: string): string {
  return raw
    .replace(PARSER_EDGE_STRIP, "")
    .replace(/[\t\n\r]/g, "")
    .replace(/^[A-Za-z][A-Za-z0-9+.-]*:[/\\]*/, "")
    .replace(/^[^/\\?#]*/, "");
}

function resolveLogtoEndpoint(value: string): string {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new Error("PUBLIC_LOGTO_ENDPOINT is not a valid URL");
  }
  if (url.username !== "" || url.password !== "") {
    throw new Error("PUBLIC_LOGTO_ENDPOINT must not contain credentials");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new Error("PUBLIC_LOGTO_ENDPOINT must use http or https");
  }
  // `url.pathname` hides dot segments and backslashes, so the raw text after
  // the authority is checked too. The message never echoes the value.
  const rawPath = rawPathAfterAuthority(value);
  if (
    url.pathname !== "/" ||
    value.includes("?") ||
    value.includes("#") ||
    (rawPath !== "" && rawPath !== "/")
  ) {
    throw new Error(
      "PUBLIC_LOGTO_ENDPOINT must be an origin without a path, query or fragment",
    );
  }
  return url.origin;
}

function readPaddleSettings(
  env: AccountsAppBuildEnv,
): PaddleSettings | undefined {
  const clientToken = (env.PUBLIC_PADDLE_CLIENT_TOKEN ?? "").trim();
  const rawEnvironment = (env.PUBLIC_PADDLE_ENVIRONMENT ?? "").trim();
  let environment: PaddleEnvironment | undefined;
  if (rawEnvironment === PaddleEnvironment.Sandbox) {
    environment = PaddleEnvironment.Sandbox;
  } else if (rawEnvironment === PaddleEnvironment.Production) {
    environment = PaddleEnvironment.Production;
  } else if (rawEnvironment !== "") {
    throw new Error("PUBLIC_PADDLE_ENVIRONMENT must be sandbox or production");
  }
  if (clientToken === "") {
    return undefined;
  }
  if (environment === undefined) {
    throw new Error(
      "PUBLIC_PADDLE_ENVIRONMENT must be set with PUBLIC_PADDLE_CLIENT_TOKEN",
    );
  }
  const prefix = environment === PaddleEnvironment.Sandbox ? "test_" : "live_";
  if (!clientToken.startsWith(prefix) || /\s/.test(clientToken)) {
    throw new Error(
      `PUBLIC_PADDLE_CLIENT_TOKEN must be a Paddle ${environment} client-side token (${prefix}...)`,
    );
  }
  return { clientToken, environment };
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
  // therefore checked too.
  const rawPath = rawPathAfterAuthority(raw);
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
