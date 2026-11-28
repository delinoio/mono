// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { useEffect, useRef, useState } from "react";

/** The state of a long-running server stream. */
export enum StreamStatus {
  Connecting = "connecting",
  Live = "live",
  /** The page is hidden, so the stream is closed until it is visible. */
  Paused = "paused",
  /** A retryable failure; the stream connects again after a delay. */
  Reconnecting = "reconnecting",
  /** A terminal failure; the stream stays closed. */
  Failed = "failed",
}

/** Errors after which a reconnect cannot succeed. */
export function isTerminalStreamError(error: unknown): boolean {
  return (
    error instanceof ConnectError &&
    (error.code === Code.NotFound ||
      error.code === Code.PermissionDenied ||
      error.code === Code.Unauthenticated ||
      error.code === Code.InvalidArgument ||
      error.code === Code.FailedPrecondition)
  );
}

/** The delay before reconnect attempt `n` (from 0): 1 s doubling up to 30 s. */
export function reconnectDelayMs(attempt: number): number {
  return Math.min(30_000, 1_000 * 2 ** attempt);
}

/** Whether the page is visible, from the Page Visibility API. */
export function usePageVisible(): boolean {
  const [visible, setVisible] = useState(
    () =>
      typeof document === "undefined" || document.visibilityState !== "hidden",
  );
  useEffect(() => {
    const update = () => setVisible(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", update);
    return () => document.removeEventListener("visibilitychange", update);
  }, []);
  return visible;
}

/**
 * Keeps a server stream open: `open` starts one stream and resolves when it
 * ends cleanly (the server rotates streams before 14 minutes); the hook then
 * opens it again at once. After a retryable error it waits with backoff;
 * after a terminal error it stops and reports the error. `retry` starts it
 * again after a terminal error.
 */
export function useStream(
  open: (signal: AbortSignal, onLive: () => void) => Promise<void>,
  options: { enabled: boolean; key: string },
) {
  const [status, setStatus] = useState(StreamStatus.Connecting);
  const [error, setError] = useState<unknown>();
  const [generation, setGeneration] = useState(0);
  const openRef = useRef(open);
  openRef.current = open;

  useEffect(() => {
    if (!options.enabled) {
      setStatus(StreamStatus.Paused);
      return;
    }
    const controller = new AbortController();
    const { signal } = controller;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const wait = (ms: number) =>
      new Promise<void>((resolve) => {
        timer = setTimeout(resolve, ms);
      });
    void (async () => {
      let attempt = 0;
      setError(undefined);
      setStatus(StreamStatus.Connecting);
      while (!signal.aborted) {
        const started = Date.now();
        try {
          await openRef.current(signal, () => {
            attempt = 0;
            setError(undefined);
            setStatus(StreamStatus.Live);
          });
          // A clean end: connect again at once, but not in a tight loop
          // when a server ends every stream right away.
          if (Date.now() - started < 1_000) await wait(1_000);
        } catch (e) {
          if (signal.aborted) return;
          if (isTerminalStreamError(e)) {
            setError(e);
            setStatus(StreamStatus.Failed);
            return;
          }
          setError(e);
          setStatus(StreamStatus.Reconnecting);
          await wait(reconnectDelayMs(attempt));
          attempt++;
        }
      }
    })();
    return () => {
      controller.abort();
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [options.enabled, options.key, generation]);

  return {
    status,
    error,
    retry: () => setGeneration((g) => g + 1),
  };
}
