// SPDX-License-Identifier: Apache-2.0
import { invoke, isTauri } from "@tauri-apps/api/core";

// Mirrors `WebviewRuntime` in `src-tauri/src/app_info.rs`.
export enum WebviewRuntime {
  Cef = "cef",
  Wry = "wry",
}

// Mirrors `PlatformFamily` in `src-tauri/src/app_info.rs`.
export enum PlatformFamily {
  Desktop = "desktop",
  Mobile = "mobile",
}

// Mirrors `AppInfo` in `src-tauri/src/app_info.rs`.
export interface AppInfo {
  version: string;
  runtime: WebviewRuntime;
  platform: PlatformFamily;
  os: string;
}

// Command names registered by `tauri::generate_handler!` in `src-tauri/src/lib.rs`.
enum Command {
  AppInfo = "app_info",
}

// Returns `null` when the page runs in a plain browser, for example the
// Rsbuild development server opened without the Tauri shell.
export async function loadAppInfo(): Promise<AppInfo | null> {
  if (!isTauri()) {
    return null;
  }
  return invoke<AppInfo>(Command.AppInfo);
}
