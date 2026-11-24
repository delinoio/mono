// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { redactError, TideConfigError } from "./errors.ts";

/** Production origin of the tide control plane. */
export const DEFAULT_API_ORIGIN = "https://tide.api.delino.io";

/**
 * The Logto API resource indicator of the control plane. A Logto access
 * token for the SDK must name it as its audience.
 */
export const API_RESOURCE = "https://tide.api.delino.io";

/**
 * The credential of a client: a Logto access token for the API resource
 * `https://tide.api.delino.io`, or a personal API key (`tide_...`). A
 * function is called before every call, so it can return a fresh access
 * token; the SDK stores no token.
 */
export type TokenSource = string | (() => string | Promise<string>);

/**
 * Validates an API origin and returns it in canonical form. Write an http or
 * https origin as scheme://host[:port][/], without credentials, path, query,
 * fragment or backslash.
 */
export function resolveApiOrigin(value: string | undefined): string {
  const raw = value === undefined || value === "" ? DEFAULT_API_ORIGIN : value;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new TideConfigError("origin is not a valid URL");
  }
  if (url.username !== "" || url.password !== "") {
    throw new TideConfigError("origin must not contain credentials");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new TideConfigError("origin must use http or https");
  }
  // Check the written form because the URL parser can normalize a path to "/".
  if (
    url.pathname !== "/" ||
    url.search !== "" ||
    url.hash !== "" ||
    raw.includes("?") ||
    raw.includes("#") ||
    !/^https?:\/\/[^/?#\\]+\/?$/i.test(raw.trim())
  ) {
    throw new TideConfigError("origin must not have a path, query or fragment");
  }
  return url.origin;
}

export interface ConnectionOptions {
  origin: string;
  token: TokenSource;
  fetch: typeof globalThis.fetch;
}

/**
 * Creates the Connect transport of a client: binary Connect protocol to the
 * configured origin. It sends the token only to that origin and redacts the
 * token from errors.
 */
export function createApiTransport(options: ConnectionOptions): Transport {
  if (typeof options.token === "string" && options.token === "") {
    throw new TideConfigError("token must not be empty");
  }
  return createConnectTransport({
    baseUrl: options.origin,
    useBinaryFormat: true,
    fetch: guardedFetch(options.origin, options.fetch),
    interceptors: [credentialInterceptor(options)],
  });
}

async function currentToken(
  token: TokenSource,
  signal: AbortSignal,
): Promise<string> {
  if (typeof token === "string") {
    return token;
  }
  const value = await untilAborted(
    new Promise<string>((resolve) => resolve(token())),
    signal,
  );
  if (typeof value !== "string" || value === "") {
    throw new TideConfigError("the token source returned no token");
  }
  return value;
}

/**
 * Waits for a promise, but rejects with the reason of the signal when the
 * signal aborts first. A late result of the promise is then ignored, so a
 * token source that answers after the abort starts no request.
 */
function untilAborted<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) {
    // A token source can abort synchronously before returning or throwing.
    // Observe its rejection even though the call has already been canceled.
    void promise.catch(() => {});
    return Promise.reject(signal.reason);
  }
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(signal.reason);
    signal.addEventListener("abort", onAbort, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener("abort", onAbort);
        resolve(value);
      },
      (error: unknown) => {
        signal.removeEventListener("abort", onAbort);
        reject(error);
      },
    );
  });
}

function credentialInterceptor(options: ConnectionOptions): Interceptor {
  return (next) => async (req) => {
    if (new URL(req.url).origin !== options.origin) {
      throw new ConnectError(
        "refusing to send the token to another origin",
        Code.PermissionDenied,
      );
    }
    req.signal.throwIfAborted();
    const secret = await currentToken(options.token, req.signal);
    const token = secret.trim();
    try {
      req.header.set("Authorization", `Bearer ${token}`);
    } catch {
      // The TypeError of Headers.set contains the whole header value, so it
      // is not kept as the cause.
      throw new TideConfigError("the token is not a valid header value");
    }
    const secrets = [secret, token];
    let res;
    try {
      res = await next(req);
    } catch (error) {
      throw redactError(error, secrets);
    }
    if (!res.stream) {
      return res;
    }
    const inner = res.message;
    return {
      ...res,
      message: (async function* () {
        try {
          yield* inner;
        } catch (error) {
          throw redactError(error, secrets);
        }
      })(),
    };
  };
}

/**
 * A fetch that refuses every URL outside the API origin, as a second guard
 * behind the credential interceptor.
 */
export function guardedFetch(
  origin: string,
  fetch: typeof globalThis.fetch,
): typeof globalThis.fetch {
  return async (input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.href
          : input.url;
    if (new URL(url).origin !== origin) {
      throw new TypeError("request to an origin other than the API origin");
    }
    return fetch(input, init);
  };
}
