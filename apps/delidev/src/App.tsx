// SPDX-License-Identifier: Apache-2.0
import { useEffect, useState } from "react";
import { type AppInfo, loadAppInfo, WebviewRuntime } from "./app-info.ts";

enum LoadState {
  Loading = "loading",
  Ready = "ready",
  Browser = "browser",
  Failed = "failed",
}

type State =
  | { kind: LoadState.Loading }
  | { kind: LoadState.Ready; info: AppInfo }
  | { kind: LoadState.Browser }
  | { kind: LoadState.Failed; message: string };

const runtimeLabels: Record<WebviewRuntime, string> = {
  [WebviewRuntime.Cef]: "Chromium Embedded Framework",
  [WebviewRuntime.Wry]: "System webview",
};

export function App() {
  const [state, setState] = useState<State>({ kind: LoadState.Loading });

  useEffect(() => {
    let active = true;
    loadAppInfo().then(
      (info) => {
        if (!active) return;
        setState(
          info ? { kind: LoadState.Ready, info } : { kind: LoadState.Browser },
        );
      },
      (error: unknown) => {
        if (!active) return;
        console.error("app_info command failed", error);
        setState({ kind: LoadState.Failed, message: String(error) });
      },
    );
    return () => {
      active = false;
    };
  }, []);

  return (
    <main className="shell">
      <h1>DeliDev</h1>
      <AppInfoPanel state={state} />
    </main>
  );
}

function AppInfoPanel({ state }: { state: State }) {
  switch (state.kind) {
    case LoadState.Loading:
      return <p aria-busy="true">Loading app details…</p>;
    case LoadState.Browser:
      return (
        <p>
          Running in a browser. Start the app with Tauri to see the runtime.
        </p>
      );
    case LoadState.Failed:
      return <p role="alert">Could not load app details: {state.message}</p>;
    case LoadState.Ready:
      return (
        <dl className="details">
          <dt>Version</dt>
          <dd>{state.info.version}</dd>
          <dt>Webview runtime</dt>
          <dd>{runtimeLabels[state.info.runtime]}</dd>
          <dt>Platform</dt>
          <dd>
            {state.info.platform} ({state.info.os})
          </dd>
        </dl>
      );
  }
}
