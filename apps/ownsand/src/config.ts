// SPDX-License-Identifier: Apache-2.0

/** Production origin of the OwnSand control plane. */
export const DEFAULT_API_ORIGIN = "https://ownsand.api.delino.io";

/** The Logto API resource indicator of the control plane. */
export const API_RESOURCE = "https://ownsand.api.delino.io";

/**
 * The API resource scope that the control plane requires in every user
 * access token. The console always requests it.
 */
export const REQUIRED_API_SCOPE = "ownsand:use";

/** Account settings live in delino-accounts. */
export const ACCOUNTS_URL = "https://accounts.delino.io";

/** How the console signs people in. */
export enum AuthMode {
  /** Logto single-page application sign-in. */
  Logto = "logto",
  /**
   * An in-browser fake API with a fake signed-in user, for the development
   * server and the end-to-end suite only. Production builds reject it.
   */
  Mock = "mock",
}

/** Build-time variables of the console. All are public. */
export interface ConsoleBuildEnv {
  readonly PUBLIC_API_ORIGIN?: string;
  readonly PUBLIC_LOGTO_ENDPOINT?: string;
  readonly PUBLIC_LOGTO_APP_ID?: string;
  readonly PUBLIC_LOGTO_SCOPES?: string;
  readonly PUBLIC_OWNSAND_MOCK_API?: string;
}

export interface LogtoSettings {
  endpoint: string;
  appId: string;
  scopes: string[];
}

export interface ConsoleConfig {
  apiOrigin: string;
  authMode: AuthMode;
  /** Undefined when the Logto registration is not configured. */
  logto: LogtoSettings | undefined;
}

/** Validates an http or https origin without path, query, fragment or credentials. */
export function resolveOrigin(
  name: string,
  value: string | undefined,
  fallback: string,
): string {
  const raw =
    value === undefined || /^[\t-\r ]*$/.test(value) ? fallback : value;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new Error(`${name} is not a valid URL`);
  }
  if (url.username !== "" || url.password !== "") {
    throw new Error(`${name} must not contain credentials`);
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new Error(`${name} must use http or https`);
  }
  if (
    url.pathname !== "/" ||
    raw.includes("?") ||
    raw.includes("#") ||
    /^[a-z]+:\/\/[^/]+\/./i.test(raw.trim())
  ) {
    throw new Error(
      `${name} must be an origin without a path, query or fragment`,
    );
  }
  return url.origin;
}

/**
 * Normalizes the raw mock setting. `readConsoleConfig` and the value that the
 * bundler inlines for `src/index.tsx` both use it, so the auth mode and the
 * API transport always follow one decision.
 */
export function normalizeMockFlag(value: string | undefined): string {
  return (value ?? "").trim();
}

/**
 * Reads the console configuration. `production` is true for production
 * builds, which must not enable the mock API.
 */
export function readConsoleConfig(
  env: ConsoleBuildEnv,
  options: { production: boolean },
): ConsoleConfig {
  const apiOrigin = resolveOrigin(
    "PUBLIC_API_ORIGIN",
    env.PUBLIC_API_ORIGIN,
    DEFAULT_API_ORIGIN,
  );
  const mock = normalizeMockFlag(env.PUBLIC_OWNSAND_MOCK_API);
  if (mock !== "" && mock !== "true" && mock !== "false") {
    throw new Error("PUBLIC_OWNSAND_MOCK_API must be true or false");
  }
  if (mock === "true") {
    if (options.production) {
      throw new Error(
        "PUBLIC_OWNSAND_MOCK_API must not be set for a production build",
      );
    }
    return { apiOrigin, authMode: AuthMode.Mock, logto: undefined };
  }
  const endpoint = (env.PUBLIC_LOGTO_ENDPOINT ?? "").trim();
  const appId = (env.PUBLIC_LOGTO_APP_ID ?? "").trim();
  const logto =
    endpoint === "" || appId === ""
      ? undefined
      : {
          endpoint: resolveOrigin("PUBLIC_LOGTO_ENDPOINT", endpoint, endpoint),
          appId,
          scopes: [
            REQUIRED_API_SCOPE,
            ...(env.PUBLIC_LOGTO_SCOPES ?? "")
              .split(/\s+/)
              .filter((scope) => scope !== "" && scope !== REQUIRED_API_SCOPE),
          ],
        };
  return { apiOrigin, authMode: AuthMode.Logto, logto };
}
