// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { redactError, SandRunConfigError } from "./errors.ts";

/** Production origin of the SandRun control plane. */
export const DEFAULT_API_ORIGIN = "https://sandrun.api.delino.io";

/**
 * The Logto API resource indicator of the control plane. A Logto access
 * token for the SDK must name it as its audience.
 */
export const API_RESOURCE = "https://sandrun.api.delino.io";

/**
 * The credential of a client: a Logto access token for the API resource
 * `https://sandrun.api.delino.io`, or a personal API key (`sandrun_...`). A
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
    throw new SandRunConfigError("origin is not a valid URL");
  }
  if (url.username !== "" || url.password !== "") {
    throw new SandRunConfigError("origin must not contain credentials");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new SandRunConfigError("origin must use http or https");
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
    throw new SandRunConfigError(
      "origin must not have a path, query or fragment",
    );
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
  if (typeof options.token === "string" && options.token.trim() === "") {
    throw new SandRunConfigError("token must not be empty");
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
  const value = await waitForToken(
    new Promise<string>((resolve) => resolve(token())),
    signal,
  );
  if (typeof value !== "string" || value.trim() === "") {
    throw new SandRunConfigError("the token source returned no token");
  }
  return value;
}

/**
 * Waits for the token and reports provider failures as configuration errors.
 * If the signal aborts first, it rejects with the signal's reason and ignores
 * the provider's result.
 */
function waitForToken(
  promise: Promise<string>,
  signal: AbortSignal,
): Promise<string> {
  if (signal.aborted) {
    // A token source can abort synchronously before returning or throwing.
    // Observe its rejection even though the call has already been canceled.
    void promise.catch(() => {});
    return Promise.reject(signal.reason);
  }
  return new Promise<string>((resolve, reject) => {
    const onAbort = () => reject(signal.reason);
    signal.addEventListener("abort", onAbort, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener("abort", onAbort);
        resolve(value);
      },
      (cause: unknown) => {
        signal.removeEventListener("abort", onAbort);
        reject(new SandRunConfigError("the token source failed", { cause }));
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
      throw new SandRunConfigError("the token is not a valid header value");
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
