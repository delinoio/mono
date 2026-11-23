// SPDX-License-Identifier: Apache-2.0
import { createContext, type ReactNode, useContext } from "react";
import type { Paddle } from "@paddle/paddle-js";
import type { PaddleSettings } from "./config.ts";

/**
 * Opens the Paddle checkout for a transaction that accounts-api created.
 * Payment details go only into the Paddle checkout; the app never sees them.
 */
export interface Checkout {
  /**
   * Opens the checkout overlay for `transactionId` (`txn_...`) and calls
   * `onClosed` when the user closes it or the payment completes. Rejects
   * when Paddle.js cannot load.
   *
   * When `signal` aborts before the checkout opens, including while Paddle.js
   * still loads, the call resolves without installing the event callback or
   * opening the overlay. The shared Paddle.js load is not canceled, so a later
   * call reuses it.
   */
  open(
    transactionId: string,
    onClosed: () => void,
    signal?: AbortSignal,
  ): Promise<void>;
}

/** The Paddle.js v2 (Paddle Billing) script that defines `PaddleBillingV1`. */
const PADDLE_SCRIPT_URL = "https://cdn.paddle.com/paddle/v2/paddle.js";

/**
 * Loads the Paddle.js script and resolves when its global is available.
 *
 * Workaround: `initializePaddle` of `@paddle/paddle-js` 1.6.6 keeps the
 * promise of its script load in a private module-level map and never clears
 * it, so one `error` event of the script makes every later `initializePaddle`
 * call reject without a new request, and the failed `<script>` element stays
 * in the document. This helper owns the script load instead: a failed attempt
 * removes its element and rejects without caching anything, so the next
 * attempt requests the script again. Scope: this file only; the SDK still
 * provides the types and `getPaddleInstance`. Remove this helper and use
 * `initializePaddle` again when a `@paddle/paddle-js` release clears the
 * cached promise after a failed script load.
 */
function loadPaddleScript(): Promise<void> {
  return new Promise((resolve, reject) => {
    const script = document.createElement("script");
    script.src = PADDLE_SCRIPT_URL;
    function fail(error: Error) {
      script.remove();
      reject(error);
    }
    script.addEventListener("load", () => resolve());
    script.addEventListener("error", () =>
      fail(new Error("Failed to load Paddle.js")),
    );
    const parent = document.head ?? document.body;
    if (parent === null || parent === undefined) {
      reject(new Error("Cannot inject Paddle.js without a head or body"));
      return;
    }
    parent.appendChild(script);
  });
}

/** Creates the checkout that loads Paddle.js on first use. */
export function createPaddleCheckout(settings: PaddleSettings): Checkout {
  // Paddle.js reports checkout events through the one callback that it gets
  // at initialization, so the callback of the open checkout lives here.
  let onEvent: ((name: string) => void) | undefined;
  // The initialized Paddle.js. A failed load clears it, so only a successful
  // initialization is reused and the script is requested once.
  let loading: Promise<Paddle> | undefined;
  async function load(): Promise<Paddle> {
    const { getPaddleInstance } = await import("@paddle/paddle-js");
    let paddle = getPaddleInstance("v1");
    if (paddle === undefined) {
      await loadPaddleScript();
      paddle = getPaddleInstance("v1");
    }
    if (paddle === undefined) {
      throw new Error("Paddle checkout could not load");
    }
    paddle.Environment.set(settings.environment);
    paddle.Initialize({
      token: settings.clientToken,
      eventCallback: (event) => {
        if (event.name !== undefined) onEvent?.(event.name);
      },
    });
    return paddle;
  }
  return {
    async open(transactionId, onClosed, signal) {
      loading ??= load();
      const attempt = loading;
      let paddle: Paddle;
      try {
        paddle = await attempt;
      } catch (error) {
        if (loading === attempt) loading = undefined;
        throw error;
      }
      // The caller left while Paddle.js loaded: the wait above is the second
      // point at which a stale request can reach the overlay.
      if (signal?.aborted) return;
      onEvent = (name) => {
        if (name === "checkout.closed" || name === "checkout.completed") {
          onClosed();
        }
      };
      paddle.Checkout.open({ transactionId });
    },
  };
}

const CheckoutContext = createContext<Checkout | undefined>(undefined);

export function CheckoutProvider({
  checkout,
  children,
}: {
  checkout: Checkout | undefined;
  children: ReactNode;
}) {
  return (
    <CheckoutContext.Provider value={checkout}>
      {children}
    </CheckoutContext.Provider>
  );
}

/** The checkout, or undefined when this build has no Paddle configuration. */
export function useCheckout(): Checkout | undefined {
  return useContext(CheckoutContext);
}
