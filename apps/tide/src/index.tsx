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

async function start() {
  // The mock API is a separate chunk that only a mock build loads. The
  // condition compares the inlined build-time value, so the minifier drops
  // the import from every other build; a production build also rejects the
  // mock setting (src/config.ts).
  const mockTransport =
    import.meta.env.PUBLIC_TIDE_MOCK_API === "true" &&
    CONSOLE_CONFIG.authMode === AuthMode.Mock
      ? (await import("./mock/mock-api.ts")).createMockTransport({
          runnerProfileCount: 201,
        })
      : undefined;
  createRoot(root!).render(
    <StrictMode>
      <App config={CONSOLE_CONFIG} mockTransport={mockTransport} />
    </StrictMode>,
  );
}

void start();
