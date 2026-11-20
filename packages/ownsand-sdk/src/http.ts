// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import type { UniversalClientFn } from "@connectrpc/connect/protocol";

import { localError, markInterruption } from "./errors.ts";

/**
 * Internal request header that links a call to its byte-activity listener.
 * The SDK HTTP client removes it before the request leaves the process.
 */
export const ACTIVITY_HEADER = "x-ownsand-sdk-activity";

/** Header that carries the relay grant ID on `RelayService` requests. */
export const GRANT_HEADER = "Ownsand-Relay-Grant-Id";

/** Listeners that observe the arrival of response body bytes per call. */
export class ActivityRegistry {
  readonly #listeners = new Map<string, () => void>();
  #next = 0;

  register(listener: () => void): { id: string; dispose: () => void } {
    const id = String(++this.#next);
    this.#listeners.set(id, listener);
    return { id, dispose: () => this.#listeners.delete(id) };
  }

  get(id: string): (() => void) | undefined {
    return this.#listeners.get(id);
  }
}

export interface SdkHttpClientOptions {
  /** The configured API origin. Credentials go only to this origin. */
  origin: string;
  /** Returns the current credential value. */
  credential: () => Promise<string>;
  inner: UniversalClientFn;
  activity: ActivityRegistry;
}

/**
 * Wraps the HTTP client of the Connect transport. It refuses every URL
 * outside the configured origin, adds the credential, reports response body
 * byte arrival for the relay stream watchdog, and marks connection failures as
 * transport interruptions.
 */
export function createSdkHttpClient(
  options: SdkHttpClientOptions,
): UniversalClientFn {
  return async (request) => {
    let url: URL;
    try {
      url = new URL(request.url);
    } catch {
      throw localError("invalid request URL", Code.Internal);
    }
    if (url.origin !== options.origin) {
      throw localError(
        "refusing to send a request outside the configured API origin",
        Code.Internal,
      );
    }
    const header = new Headers(request.header);
    const activityId = header.get(ACTIVITY_HEADER);
    header.delete(ACTIVITY_HEADER);
    const onBytes =
      activityId === null ? undefined : options.activity.get(activityId);
    header.set("authorization", `Bearer ${await options.credential()}`);
    let response;
    try {
      response = await options.inner({ ...request, header });
    } catch (err) {
      throw markInterruptionUnlessAborted(err, request.signal);
    }
    onBytes?.();
    return {
      ...response,
      body: observeBody(response.body, onBytes, request.signal),
    };
  };
}

async function* observeBody(
  body: AsyncIterable<Uint8Array>,
  onBytes: (() => void) | undefined,
  signal: AbortSignal | undefined,
): AsyncIterable<Uint8Array> {
  try {
    for await (const chunk of body) {
      if (chunk.byteLength > 0) {
        onBytes?.();
      }
      yield chunk;
    }
  } catch (err) {
    throw markInterruptionUnlessAborted(err, signal);
  }
}

function markInterruptionUnlessAborted(
  err: unknown,
  signal: AbortSignal | undefined,
): unknown {
  if (signal?.aborted || typeof err !== "object" || err === null) {
    return err;
  }
  return markInterruption(err);
}
