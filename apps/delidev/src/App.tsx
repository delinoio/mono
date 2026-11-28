// SPDX-License-Identifier: Apache-2.0
import { useEffect, useState } from "react";
import { type AppInfo, loadAppInfo, WebviewRuntime } from "./app-info.ts";
import { useI18n } from "./i18n.ts";

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

const runtimeLabels = {
  [WebviewRuntime.Cef]: "runtime.cef",
  [WebviewRuntime.Wry]: "runtime.wry",
} as const satisfies Record<WebviewRuntime, string>;

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
      <header className="header">
        <h1>DeliDev</h1>
        <LanguageSelect />
      </header>
      <AppInfoPanel state={state} />
    </main>
  );
}

function LanguageSelect() {
  const { lang, locales, setLanguage, t } = useI18n();
  return (
    <label className="language">
      <span>{t("app.language")}</span>
      <select
        value={lang}
        onChange={(event) => {
          setLanguage(event.target.value).catch((error: unknown) => {
            console.error("language selection failed", error);
          });
        }}
      >
        {locales.map((locale) => (
          <option key={locale.lang} value={locale.lang} lang={locale.lang}>
            {locale.label}
          </option>
        ))}
      </select>
    </label>
  );
}

function AppInfoPanel({ state }: { state: State }) {
  const { t } = useI18n();
  switch (state.kind) {
    case LoadState.Loading:
      return <p aria-busy="true">{t("appInfo.loading")}</p>;
    case LoadState.Browser:
      return <p>{t("appInfo.browser")}</p>;
    case LoadState.Failed:
      return (
        <p role="alert">{t("appInfo.failed", { message: state.message })}</p>
      );
    case LoadState.Ready:
      return (
        <dl className="details">
          <dt>{t("appInfo.version")}</dt>
          <dd>{state.info.version}</dd>
          <dt>{t("appInfo.runtime")}</dt>
          <dd>{t(runtimeLabels[state.info.runtime])}</dd>
          <dt>{t("appInfo.platform")}</dt>
          <dd>
            {t("appInfo.platformValue", {
              platform: state.info.platform,
              os: state.info.os,
            })}
          </dd>
        </dl>
      );
  }
}
