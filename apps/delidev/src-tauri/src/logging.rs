// SPDX-License-Identifier: Apache-2.0

use tracing_subscriber::EnvFilter;

/// Environment variable that holds the `tracing` filter directives.
const LOG_FILTER_ENV: &str = "DELIDEV_LOG";
/// Filter used when `DELIDEV_LOG` is unset or invalid.
const DEFAULT_LOG_FILTER: &str = "info";

/// Installs the structured stderr logger.
///
/// ANSI color is on by default. A non-empty `NO_COLOR` turns it off
/// (<https://no-color.org>). Android and iOS do not show the process stderr;
/// a platform log sink for mobile is not part of the scaffold.
pub fn init() {
    let filter = EnvFilter::try_from_env(LOG_FILTER_ENV)
        .unwrap_or_else(|_| EnvFilter::new(DEFAULT_LOG_FILTER));
    let ansi = std::env::var_os("NO_COLOR").is_none_or(|value| value.is_empty());

    // `try_init` fails only when a global subscriber already exists, which is
    // harmless: the earlier subscriber keeps receiving the events.
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_ansi(ansi)
        .with_writer(std::io::stderr)
        .try_init();
}
