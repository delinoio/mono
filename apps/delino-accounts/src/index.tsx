// SPDX-License-Identifier: Apache-2.0
import "delino-ui/styles.css";
import "./index.css";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App.tsx";
import { ACCOUNTS_API_ORIGIN } from "./api-origin.ts";
import { ACCOUNTS_CONFIG } from "./build-config.ts";
import { I18nProvider, loadInitialLanguage } from "./i18n.ts";

// Expose the inlined API origin on the document root so the build-time value
// is observable in the running app; the e2e suite reads it. This is not
// user-visible markup.
document.documentElement.dataset.apiOrigin = ACCOUNTS_API_ORIGIN;

const root = document.getElementById("root");
if (!root) {
  throw new Error("Missing #root element");
}

// Load the selected catalog before the first render, so the first render
// shows the selected language.
const initial = await loadInitialLanguage();

// The provider is above the auth provider, the router and the checkout, so
// every page, including the sign-in callback, uses the selected language.
createRoot(root).render(
  <StrictMode>
    <I18nProvider initial={initial}>
      <App config={ACCOUNTS_CONFIG} />
    </I18nProvider>
  </StrictMode>,
);
