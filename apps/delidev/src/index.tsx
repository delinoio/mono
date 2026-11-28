// SPDX-License-Identifier: Apache-2.0
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App.tsx";
import { I18nProvider, loadInitialLanguage } from "./i18n.ts";
import "./index.css";

const root = document.getElementById("root");
if (!root) {
  throw new Error("Missing #root element");
}

// Load the selected catalog before the first render, so the first render
// shows the selected language.
const initial = await loadInitialLanguage();

createRoot(root).render(
  <StrictMode>
    <I18nProvider initial={initial}>
      <App />
    </I18nProvider>
  </StrictMode>,
);
