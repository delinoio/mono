// SPDX-License-Identifier: Apache-2.0

//! DeliDev app shell shared by the desktop and mobile targets.
//!
//! Desktop targets (Linux, macOS and Windows) run on the Chromium Embedded
//! Framework through `tauri-runtime-cef`. Android and iOS run on the system
//! webview through `tauri-runtime-wry`, because CEF has no mobile port. The
//! `desktop` and `mobile` cfg aliases come from `tauri-build`.

mod app_info;
mod logging;

use app_info::AppInfo;

/// Returns the build and runtime details that the frontend shows.
#[tauri::command]
fn app_info() -> AppInfo {
    AppInfo::current()
}

/// Starts the app.
///
/// On desktop, `cef_entry_point` handles the CEF helper processes first: CEF
/// relaunches this executable as its renderer, GPU, network and utility
/// processes with a `--type=` switch, and the attribute makes those processes
/// run the helper and return before the Tauri app is built.
#[cfg_attr(mobile, tauri::mobile_entry_point)]
#[cfg_attr(desktop, tauri_runtime_cef::cef_entry_point)]
pub fn run() {
    logging::init();
    let info = AppInfo::current();
    tracing::info!(
        version = info.version,
        runtime = ?info.runtime,
        platform = ?info.platform,
        os = info.os,
        "starting delidev"
    );

    runtime_builder()
        .invoke_handler(tauri::generate_handler![app_info])
        .run(tauri::generate_context!())
        .expect("error while running the delidev app");
}

/// Selects the CEF runtime on desktop targets.
#[cfg(desktop)]
fn runtime_builder() -> tauri::Builder<tauri::DynRuntime> {
    tauri::Builder::default().runtime(tauri_runtime_cef::Cef::default())
}

/// Selects the system webview runtime on mobile targets.
#[cfg(mobile)]
fn runtime_builder() -> tauri::Builder<tauri::DynRuntime> {
    tauri::Builder::default().runtime(tauri_runtime_wry::Wry::default())
}
