// SPDX-License-Identifier: Apache-2.0
import { createContext, type ReactNode, useContext } from "react";
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
   */
  open(transactionId: string, onClosed: () => void): Promise<void>;
}

/** Creates the checkout that loads Paddle.js on first use. */
export function createPaddleCheckout(settings: PaddleSettings): Checkout {
  // Paddle.js reports checkout events through the one callback that it gets
  // at initialization, so the callback of the open checkout lives here.
  let onEvent: ((name: string) => void) | undefined;
  let loading: ReturnType<typeof load> | undefined;
  function load() {
    return import("@paddle/paddle-js").then(({ initializePaddle }) =>
      initializePaddle({
        environment: settings.environment,
        token: settings.clientToken,
        eventCallback: (event) => {
          if (event.name !== undefined) onEvent?.(event.name);
        },
      }),
    );
  }
  return {
    async open(transactionId, onClosed) {
      loading ??= load();
      let paddle: Awaited<ReturnType<typeof load>>;
      try {
        paddle = await loading;
      } catch (error) {
        loading = undefined;
        throw error;
      }
      if (paddle === undefined) {
        loading = undefined;
        throw new Error("Paddle checkout could not load");
      }
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
