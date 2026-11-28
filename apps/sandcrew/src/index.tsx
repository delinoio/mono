// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "./index.css";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App.tsx";
import { CONSOLE_CONFIG } from "./build-config.ts";
import { AuthMode } from "./config.ts";

const root = document.getElementById("root");
if (!root) {
  throw new Error("Missing #root element");
}

/**
 * The response delay of the mock API, so the end-to-end suite can show the
 * loading states. Only mock builds read it.
 */
function mockResponseDelayMs(): number {
  try {
    const value = Number(
      window.sessionStorage.getItem("sandcrew.mock-response-delay-ms") ?? "0",
    );
    return Number.isFinite(value) && value > 0 ? value : 0;
  } catch {
    return 0;
  }
}

async function start() {
  // The mock API is a separate chunk that only a mock build loads. The
  // condition compares the inlined build-time value, so the minifier drops
  // the import from every other build; a production build also rejects the
  // mock setting (src/config.ts).
  const mockTransport =
    import.meta.env.PUBLIC_SANDCREW_MOCK_API === "true" &&
    CONSOLE_CONFIG.authMode === AuthMode.Mock
      ? (await import("./mock/mock-api.ts")).createMockTransport({
          responseDelayMs: mockResponseDelayMs(),
        })
      : undefined;
  createRoot(root!).render(
    <StrictMode>
      <App config={CONSOLE_CONFIG} mockTransport={mockTransport} />
    </StrictMode>,
  );
}

void start();
