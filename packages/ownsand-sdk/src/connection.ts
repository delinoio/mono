// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  type Interceptor,
  type Transport,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  OwnSandConfigError,
  OwnSandCredentialError,
  redactError,
} from "./errors.ts";
import { type Logger, LogEvent, safeLog } from "./logger.ts";

/** Production origin of the OwnSand control plane. */
export const DEFAULT_API_ORIGIN = "https://ownsand.api.delino.io";

/** Request header that carries the relay grant ID on relay RPCs. */
export const RELAY_GRANT_HEADER = "Ownsand-Relay-Grant-Id";

/**
 * Internal header that links a call to its byte activity listener. The
 * guarded fetch removes it before the request leaves the process.
 */
export const ACTIVITY_HEADER = "x-ownsand-sdk-activity";

/** One credential: an organization API key or a Logto access token source. */
export type Credential =
  | { readonly apiKey: string }
  | { readonly accessToken: () => string | Promise<string> };

/** Environment variables that the SDK reads. */
export enum EnvVar {
  ApiKey = "OWNSAND_API_KEY",
  Organization = "OWNSAND_ORG",
}

export type Env = Readonly<Record<string, string | undefined>>;

/** Reads the process environment when the runtime has one. */
export function processEnv(): Env {
  const p = (globalThis as { process?: { env?: Env } }).process;
  return p?.env ?? {};
}

/**
 * Validates an API origin and returns it in canonical form. It must be an
 * http or https origin without credentials, path, query or fragment.
 */
export function resolveApiOrigin(value: string | undefined): string {
  const raw = value === undefined || value === "" ? DEFAULT_API_ORIGIN : value;
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new OwnSandConfigError("origin is not a valid URL");
  }
  if (url.username !== "" || url.password !== "") {
    throw new OwnSandConfigError("origin must not contain credentials");
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new OwnSandConfigError("origin must use http or https");
  }
  if (
    url.pathname !== "/" ||
    url.search !== "" ||
    url.hash !== "" ||
    raw.includes("?") ||
    raw.includes("#")
  ) {
    throw new OwnSandConfigError(
      "origin must not have a path, query or fragment",
    );
  }
  return url.origin;
}

/** Resolves the explicit credential or `OWNSAND_API_KEY`. */
export function resolveCredential(
  credential: Credential | undefined,
  env: Env,
): Credential {
  if (credential !== undefined) {
    if ("apiKey" in credential && credential.apiKey === "") {
      throw new OwnSandConfigError("apiKey must not be empty");
    }
    return credential;
  }
  const apiKey = env[EnvVar.ApiKey];
  if (apiKey !== undefined && apiKey !== "") {
    return { apiKey };
  }
  throw new OwnSandConfigError(
    `no credential: pass apiKey or accessToken, or set ${EnvVar.ApiKey}`,
  );
}

/** Tracks the bytes that arrive on the response body of a call. */
export class ActivityRegistry {
  readonly #listeners = new Map<string, () => void>();
  #next = 0;

  register(listener: () => void): { id: string; dispose: () => void } {
    const id = String(++this.#next);
    this.#listeners.set(id, listener);
    return { id, dispose: () => this.#listeners.delete(id) };
  }

  notify(id: string): void {
    this.#listeners.get(id)?.();
  }
}

export interface ConnectionOptions {
  origin: string;
  credential: Credential;
  organizationId: string | undefined;
  fetch: typeof globalThis.fetch;
  logger: Logger | undefined;
  activity: ActivityRegistry;
  now: () => number;
}

/**
 * Creates the Connect transport of a client. It sends the credential only to
 * the configured origin, fills a missing `organization_id` from the client
 * default, fails before sending when an RPC needs an organization that is
 * not set, reports byte activity and redacts the credential from errors.
 */
export function createApiTransport(options: ConnectionOptions): Transport {
  return createConnectTransport({
    baseUrl: options.origin,
    useBinaryFormat: true,
    fetch: guardedFetch(options),
    interceptors: [
      loggingInterceptor(options),
      redactingInterceptor(options),
      organizationInterceptor(options.organizationId),
      credentialInterceptor(options),
    ],
  });
}

/**
 * Returns the secret of one request. An access token callback can take
 * arbitrarily long, so the wait ends as soon as the request signal aborts:
 * a caller cancellation or the authorization deadline. A callback that
 * resolves after the abort is ignored and no RPC starts.
 */
async function currentSecret(
  credential: Credential,
  signal: AbortSignal,
): Promise<string> {
  if ("apiKey" in credential) {
    return credential.apiKey;
  }
  signal.throwIfAborted();
  // A synchronous throw of the callback stays a rejection of the race. A
  // failure of the callback is a terminal credential error that keeps the
  // callback's message; the abort reason of the race stays unwrapped.
  const token = await raceAbort(
    new Promise<string>((resolve, reject) => {
      const fail = (error: unknown) =>
        reject(
          new OwnSandCredentialError(
            error instanceof Error ? error.message : String(error),
            { cause: error },
          ),
        );
      try {
        Promise.resolve(credential.accessToken()).then(resolve, fail);
      } catch (error) {
        fail(error);
      }
    }),
    signal,
  );
  if (typeof token !== "string" || token === "") {
    throw new OwnSandCredentialError("accessToken returned no token");
  }
  return token;
}

/**
 * Settles with the promise or rejects with the abort reason of the signal,
 * whichever comes first. The single abort listener is removed on settlement,
 * and a later rejection of the losing promise is swallowed so that it does
 * not surface as an unhandled rejection.
 */
function raceAbort<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => {
      signal.removeEventListener("abort", onAbort);
      reject(signal.reason);
    };
    signal.addEventListener("abort", onAbort);
    promise.then(
      (value) => {
        signal.removeEventListener("abort", onAbort);
        resolve(value);
      },
      (error) => {
        signal.removeEventListener("abort", onAbort);
        reject(error);
      },
    );
    if (signal.aborted) {
      onAbort();
    }
  });
}

function credentialInterceptor(options: ConnectionOptions): Interceptor {
  return (next) => async (req) => {
    if (new URL(req.url).origin !== options.origin) {
      throw new ConnectError(
        "refusing to send the credential to another origin",
        Code.PermissionDenied,
      );
    }
    const secret = await currentSecret(options.credential, req.signal);
    req.header.set("Authorization", `Bearer ${secret}`);
    // Redact with the exact credential of this request. The access token
    // differs per request, so the value lives only in this closure and never
    // in state that concurrent requests share.
    return redactCall(next, req, [secret]);
  };
}

/**
 * Calls the next handler and redacts the given secrets from errors of the
 * call, including errors that end a response stream.
 */
async function redactCall(
  next: Parameters<Interceptor>[0],
  req: Parameters<ReturnType<Interceptor>>[0],
  secrets: readonly string[],
): ReturnType<ReturnType<Interceptor>> {
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
}

function organizationInterceptor(defaultOrg: string | undefined): Interceptor {
  return (next) => async (req) => {
    if (req.stream) {
      // Every public RPC is unary or server-streaming, so the request is
      // never a client stream.
      return next(req);
    }
    if (!("organizationId" in req.method.input.field)) {
      return next(req);
    }
    const message = req.message as { organizationId?: string };
    if (message.organizationId !== undefined && message.organizationId !== "") {
      return next(req);
    }
    if (defaultOrg === undefined || defaultOrg === "") {
      throw new ConnectError(
        `${req.method.name} needs an organization: set organizationId on the client, ${EnvVar.Organization}, or the request`,
        Code.InvalidArgument,
      );
    }
    return next({
      ...req,
      message: {
        ...message,
        organizationId: defaultOrg,
      } as unknown as typeof req.message,
    });
  };
}

/**
 * Redacts the static API key. The credential interceptor redacts the
 * per-request secret, which for an access token is known only there.
 */
function redactingInterceptor(options: ConnectionOptions): Interceptor {
  const secrets = (): string[] =>
    "apiKey" in options.credential ? [options.credential.apiKey] : [];
  return (next) => (req) => redactCall(next, req, secrets());
}

function loggingInterceptor(options: ConnectionOptions): Interceptor {
  return (next) => async (req) => {
    if (options.logger === undefined) {
      return next(req);
    }
    const started = options.now();
    const rpc = `${req.service.typeName}/${req.method.name}`;
    try {
      const res = await next(req);
      safeLog(options.logger, {
        event: LogEvent.Rpc,
        rpc,
        durationMs: options.now() - started,
        code: "ok",
      });
      return res;
    } catch (error) {
      safeLog(options.logger, {
        event: LogEvent.Rpc,
        rpc,
        durationMs: options.now() - started,
        code: error instanceof ConnectError ? Code[error.code] : "unknown",
      });
      throw error;
    }
  };
}

export function guardedFetch(
  options: ConnectionOptions,
): typeof globalThis.fetch {
  return async (input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.href
          : input.url;
    if (new URL(url).origin !== options.origin) {
      throw new TypeError("request to an origin other than the API origin");
    }
    const headers = new Headers(init?.headers);
    const activityId = headers.get(ACTIVITY_HEADER);
    headers.delete(ACTIVITY_HEADER);
    const response = await options.fetch(url, { ...init, headers });
    if (activityId === null || response.body === null) {
      return response;
    }
    const registry = options.activity;
    const counted = response.body.pipeThrough(
      new TransformStream<Uint8Array, Uint8Array>({
        transform(chunk, controller) {
          registry.notify(activityId);
          controller.enqueue(chunk);
        },
      }),
    );
    return new Response(counted, {
      status: response.status,
      statusText: response.statusText,
      headers: response.headers,
    });
  };
}
