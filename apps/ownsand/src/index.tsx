// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import { StrictMode, type ComponentType } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App.tsx";
import {
  UnconfiguredAuthProvider,
  type AuthProviderProps,
} from "./auth/auth.tsx";
import { createLogtoAuthProvider } from "./auth/logto-auth.tsx";
import { CONSOLE_CONFIG } from "./build-config.ts";
import "./index.css";

async function selectAuthProvider(): Promise<ComponentType<AuthProviderProps>> {
  // `import.meta.env.DEV` is a build-time constant, so a production build
  // drops this branch and never contains the e2e stand-in.
  if (import.meta.env.DEV && CONSOLE_CONFIG.e2eFakeAuth) {
    const { FakeAuthProvider } = await import("./auth/fake-auth.tsx");
    return FakeAuthProvider;
  }
  return CONSOLE_CONFIG.logto
    ? createLogtoAuthProvider(CONSOLE_CONFIG.logto)
    : UnconfiguredAuthProvider;
}

// Expose the inlined API origin on the document root so the build-time value
// is observable in the running app (the e2e suite checks it). It is public.
document.documentElement.dataset.apiOrigin = CONSOLE_CONFIG.apiOrigin;

const root = document.getElementById("root");
if (!root) {
  throw new Error("Missing #root element");
}

void selectAuthProvider().then((AuthProvider) => {
  createRoot(root).render(
    <StrictMode>
      <App apiOrigin={CONSOLE_CONFIG.apiOrigin} AuthProvider={AuthProvider} />
    </StrictMode>,
  );
});
