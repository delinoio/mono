// SPDX-License-Identifier: Apache-2.0

#[cfg(all(windows, not(debug_assertions)))]
pub use file::init as init_file;
use tracing_subscriber::{EnvFilter, filter::LevelFilter, prelude::*};

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

/// Installs structured logging. Windows release builds can also attach a file
/// sink once the Tauri app log directory is available.
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

    let subscriber = tracing_subscriber::registry().with(filter).with(
        tracing_subscriber::fmt::layer()
            .with_ansi(ansi)
            .with_writer(std::io::stderr),
    );
    #[cfg(all(windows, not(debug_assertions)))]
    let subscriber = subscriber.with(file::layer(file::writer));

    // Keep the existing subscriber if global logging was already initialized.
    let _ = subscriber.try_init();
}

#[cfg(any(test, all(windows, not(debug_assertions))))]
mod file {
    use std::{
        fs::{self, File},
        io::{self, Write},
        path::PathBuf,
    };

    use tracing::Subscriber;
    use tracing_subscriber::{
        Layer,
        field::RecordFields,
        fmt::{
            self,
            format::{DefaultFields, FormatFields},
            writer::MakeWriter,
        },
        registry::LookupSpan,
    };

    const CURRENT_LOG: &str = "delidev.log";
    const PREVIOUS_LOG: &str = "delidev.log.1";
    const MAX_LOG_BYTES: u64 = 1024 * 1024;

    #[cfg(all(windows, not(debug_assertions)))]
    use std::sync::OnceLock;

    #[cfg(all(windows, not(debug_assertions)))]
    static LOG: OnceLock<RotatingLog> = OnceLock::new();

    #[cfg(all(windows, not(debug_assertions)))]
    pub fn init(directory: tauri::Result<PathBuf>) {
        let log = directory.and_then(|directory| RotatingLog::open(directory).map_err(Into::into));
        match log {
            Ok(log) => {
                let _ = LOG.set(log);
            }
            Err(error) => {
                let _ = writeln!(
                    io::stderr().lock(),
                    "delidev: could not initialize file logging: {error}"
                );
            }
        }
    }

    #[cfg(all(windows, not(debug_assertions)))]
    pub fn writer() -> Writer<'static> {
        Writer(LOG.get())
    }

    pub fn layer<S, W>(writer: W) -> impl Layer<S>
    where
        S: Subscriber + for<'a> LookupSpan<'a>,
        W: for<'a> MakeWriter<'a> + 'static,
    {
        fmt::layer()
            .with_ansi(false)
            .fmt_fields(FileFields::default())
            .with_writer(writer)
    }

    // The formatter caches span fields by type. A distinct type keeps stderr's
    // ANSI formatting out of the file and prevents duplicate span field
    // updates.
    #[derive(Default)]
    struct FileFields(DefaultFields);

    impl<'a> FormatFields<'a> for FileFields {
        fn format_fields<R: RecordFields>(
            &self,
            writer: fmt::format::Writer<'a>,
            fields: R,
        ) -> std::fmt::Result {
            self.0.format_fields(writer, fields)
        }
    }

    pub struct Writer<'a>(Option<&'a RotatingLog>);

    impl Write for Writer<'_> {
        fn write(&mut self, record: &[u8]) -> io::Result<usize> {
            if let Some(log) = self.0 {
                log.append(record)?;
            }
            Ok(record.len())
        }

        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    struct RotatingLog {
        directory: PathBuf,
    }

    impl RotatingLog {
        fn open(directory: PathBuf) -> io::Result<Self> {
            fs::create_dir_all(&directory)?;
            let log = Self { directory };
            log.append(b"")?;
            Ok(log)
        }

        fn append(&self, record: &[u8]) -> io::Result<()> {
            // The formatter writes one complete event at a time. Skip events
            // larger than the file limit so rotation preserves complete lines.
            let bytes = record.len() as u64;
            if bytes > MAX_LOG_BYTES {
                return Ok(());
            }

            // Lock a separate file so concurrent launches cannot write during
            // rotation. Closing this handle releases the lock.
            let lock = File::options()
                .write(true)
                .create(true)
                .truncate(false)
                .open(self.directory.join("delidev.log.lock"))?;
            lock.lock()?;

            let path = self.directory.join(CURRENT_LOG);
            let mut options = File::options();
            options.create(true).append(true);
            let mut file = options.open(&path)?;
            if file.metadata()?.len().saturating_add(bytes) > MAX_LOG_BYTES {
                drop(file);
                fs::rename(&path, self.directory.join(PREVIOUS_LOG))?;
                file = options.open(&path)?;
            }
            file.write_all(record)
        }
    }

    #[cfg(test)]
    mod tests {
        use std::time::{SystemTime, UNIX_EPOCH};

        use tracing_subscriber::prelude::*;

        use super::*;

        struct TestDirectory(PathBuf);

        impl TestDirectory {
            fn new() -> Self {
                let nonce = SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap()
                    .as_nanos();
                let path = std::env::temp_dir()
                    .join(format!("delidev-logs-{}-{nonce}", std::process::id()));
                fs::create_dir(&path).unwrap();
                Self(path)
            }
        }

        impl Drop for TestDirectory {
            fn drop(&mut self) {
                let _ = fs::remove_dir_all(&self.0);
            }
        }

        impl<'a> MakeWriter<'a> for RotatingLog {
            type Writer = Writer<'a>;

            fn make_writer(&'a self) -> Self::Writer {
                Writer(Some(self))
            }
        }

        #[test]
        fn rotates_and_keeps_only_the_latest_archive() {
            let directory = TestDirectory::new();
            let log = RotatingLog::open(directory.0.clone()).unwrap();
            let mut writer = Writer(Some(&log));
            writer
                .write_all(&vec![b'a'; MAX_LOG_BYTES as usize])
                .unwrap();
            writer
                .write_all(&vec![b'b'; MAX_LOG_BYTES as usize])
                .unwrap();
            writer.write_all(b"new event\n").unwrap();
            writer
                .write_all(&vec![b'c'; MAX_LOG_BYTES as usize + 1])
                .unwrap();

            assert_eq!(
                fs::read(directory.0.join(CURRENT_LOG)).unwrap(),
                b"new event\n"
            );
            assert_eq!(
                fs::read(directory.0.join(PREVIOUS_LOG)).unwrap(),
                vec![b'b'; MAX_LOG_BYTES as usize]
            );
        }

        #[test]
        fn reports_a_blocked_log_directory_when_opening() {
            let directory = TestDirectory::new();
            let blocked = directory.0.join("logs");
            fs::write(&blocked, b"").unwrap();

            assert!(RotatingLog::open(blocked).is_err());
        }

        #[test]
        fn writes_filtered_events_without_ansi() {
            let directory = TestDirectory::new();
            let log = RotatingLog::open(directory.0.clone()).unwrap();
            let subscriber = tracing_subscriber::registry()
                .with(super::super::log_filter(Some("warn")))
                .with(fmt::layer().with_ansi(true).with_writer(io::sink))
                .with(layer(log));
            tracing::subscriber::with_default(subscriber, || {
                let span = tracing::warn_span!("test_span", source = "test");
                let _span = span.enter();
                tracing::info!("filtered info event");
                tracing::warn!("retained warning");
            });

            let log = fs::read_to_string(directory.0.join(CURRENT_LOG)).unwrap();
            assert!(log.contains("test_span{source=\"test\"}"));
            assert!(log.contains("retained warning"));
            assert!(!log.contains("filtered info event"));
            assert!(!log.contains('\u{1b}'));
        }
    }
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
