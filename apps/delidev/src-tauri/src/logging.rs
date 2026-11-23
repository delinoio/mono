// SPDX-License-Identifier: Apache-2.0

use tracing_subscriber::{EnvFilter, filter::LevelFilter};

/// Environment variable that holds the `tracing` filter directives.
const LOG_FILTER_ENV: &str = "DELIDEV_LOG";
/// Filter used when `DELIDEV_LOG` is unset, empty or invalid.
const DEFAULT_LOG_FILTER: &str = "info";

/// Builds the event filter from the raw `DELIDEV_LOG` value.
///
/// A value without directives (empty, or only separators such as `,`) selects
/// the `info` level. Without the default directive, `tracing-subscriber`
/// parses such a value into a filter that drops every event. A value that is
/// not valid, and an unset or non-Unicode variable (`None`), also select
/// `info`.
fn log_filter(value: Option<&str>) -> EnvFilter {
    value
        .and_then(|directives| {
            EnvFilter::builder()
                .with_default_directive(LevelFilter::INFO.into())
                .parse(directives)
                .ok()
        })
        .unwrap_or_else(|| EnvFilter::new(DEFAULT_LOG_FILTER))
}

/// Installs the structured stderr logger.
///
/// ANSI color is on by default. A non-empty `NO_COLOR` turns it off
/// (<https://no-color.org>).
///
/// On iOS debug builds, the `tauri::mobile_entry_point` expansion redirects
/// stdout and stderr into pipes before the app entry point runs, and the Tauri
/// Swift logger forwards each captured chunk to Apple unified logging with an
/// `[stderr]` prefix, so this logger's output reaches unified logging. That
/// logger is off by default in iOS release builds, so the process stderr is not
/// visible there. Android does not show the process stderr, and a platform log
/// sink is not part of the scaffold. The native log visibility is not verified
/// on a device or simulator; it follows from the pinned Tauri sources.
pub fn init() {
    let filter = log_filter(std::env::var(LOG_FILTER_ENV).ok().as_deref());
    let ansi = std::env::var_os("NO_COLOR").is_none_or(|value| value.is_empty());

    // `try_init` fails only when a global subscriber already exists, which is
    // harmless: the earlier subscriber keeps receiving the events.
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_ansi(ansi)
        .with_writer(std::io::stderr)
        .try_init();
}

#[cfg(test)]
mod tests {
    use super::*;

    fn level_hint(value: Option<&str>) -> Option<LevelFilter> {
        log_filter(value).max_level_hint()
    }

    #[test]
    fn unset_variable_selects_info() {
        assert_eq!(level_hint(None), Some(LevelFilter::INFO));
    }

    #[test]
    fn empty_value_selects_info() {
        assert_eq!(level_hint(Some("")), Some(LevelFilter::INFO));
    }

    #[test]
    fn separator_only_value_selects_info() {
        assert_eq!(level_hint(Some(",")), Some(LevelFilter::INFO));
    }

    #[test]
    fn whitespace_only_value_selects_info() {
        assert_eq!(level_hint(Some(" ")), Some(LevelFilter::INFO));
    }

    #[test]
    fn explicit_level_is_kept() {
        assert_eq!(level_hint(Some("debug")), Some(LevelFilter::DEBUG));
    }

    #[test]
    fn invalid_directive_selects_info() {
        assert_eq!(
            level_hint(Some("lolwut=notalevel")),
            Some(LevelFilter::INFO)
        );
    }
}
