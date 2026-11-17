// SPDX-License-Identifier: Apache-2.0

use serde::Serialize;

/// Webview runtime that hosts the frontend.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum WebviewRuntime {
    /// Chromium Embedded Framework, used on desktop targets.
    Cef,
    /// System webview through wry, used on mobile targets.
    Wry,
}

/// Platform family of the running build.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum PlatformFamily {
    Desktop,
    Mobile,
}

/// Build and runtime details returned by the `app_info` command.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct AppInfo {
    pub version: &'static str,
    pub runtime: WebviewRuntime,
    pub platform: PlatformFamily,
    /// Operating system name from `std::env::consts::OS`.
    pub os: &'static str,
}

impl AppInfo {
    pub fn current() -> Self {
        Self {
            version: env!("CARGO_PKG_VERSION"),
            runtime: WebviewRuntime::current(),
            platform: PlatformFamily::current(),
            os: std::env::consts::OS,
        }
    }
}

impl WebviewRuntime {
    pub const fn current() -> Self {
        if cfg!(mobile) { Self::Wry } else { Self::Cef }
    }
}

impl PlatformFamily {
    pub const fn current() -> Self {
        if cfg!(mobile) {
            Self::Mobile
        } else {
            Self::Desktop
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn desktop_build_reports_cef() {
        let info = AppInfo::current();

        assert_eq!(info.runtime, WebviewRuntime::Cef);
        assert_eq!(info.platform, PlatformFamily::Desktop);
        assert_eq!(info.version, env!("CARGO_PKG_VERSION"));
    }

    #[test]
    fn serializes_with_frontend_field_names() {
        let info = AppInfo {
            version: "1.2.3",
            runtime: WebviewRuntime::Wry,
            platform: PlatformFamily::Mobile,
            os: "android",
        };

        let json = serde_json::to_value(&info).expect("AppInfo serializes");

        assert_eq!(
            json,
            serde_json::json!({
                "version": "1.2.3",
                "runtime": "wry",
                "platform": "mobile",
                "os": "android",
            })
        );
    }
}
