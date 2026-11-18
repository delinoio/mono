# apps-delidev foundation contract

## Scope

This contract owns both components of [`delidev`](project-delidev.md):

- `apps/delidev/package.json`: workspace package `delidev` with the frontend scripts and the `tauri` CLI script.
- `apps/delidev/rsbuild.config.ts`, `apps/delidev/index.html`, `apps/delidev/src/` and `apps/delidev/public/`: the `delidev-frontend` web frontend.
- `apps/delidev/turbo.json`: package-local Turbo task configuration.
- `apps/delidev/src-tauri/`: the `delidev-shell` Rust crate:
  - `Cargo.toml` and `build.rs`: crate manifest and the `tauri-build` build script.
  - `tauri.conf.json`: Tauri app configuration.
  - `capabilities/`: Tauri permission capabilities.
  - `icons/`: app icons for every bundle format, generated with `pnpm --filter delidev tauri icon public/icon.svg -o src-tauri/icons`.
  - `src/`: app shell source and unit tests.
- `apps/delidev/src-tauri/gen/schemas/`: ignored output that `tauri-build` writes on every build.

## Runtime and Language

- Frontend: Rsbuild `@rsbuild/core` 2.x with `@rsbuild/plugin-react`, React 19, TypeScript. Node.js from root `.nvmrc`.
- App shell: Rust, edition 2024, with the toolchain from root `rust-toolchain`. Minimum Rust version 1.95, from Tauri 3.
- Tauri 3 alpha, pinned to exact versions: `tauri`, `tauri-build`, `tauri-runtime-cef`, `tauri-runtime-wry`, `@tauri-apps/cli` and `@tauri-apps/api`.
- Webview runtime per target:
  - Desktop (Linux, macOS, Windows): Chromium Embedded Framework through `tauri-runtime-cef` with its `devtools` feature.
  - Mobile (Android, iOS): system webview through `tauri-runtime-wry`.
- The `desktop` and `mobile` cfg aliases come from `tauri-build`. `src/lib.rs` selects the runtime with them and is the only place that names a runtime crate.
- Deviation from repository defaults: the frontend uses Rsbuild, not Rspress, and is bundled into the native app instead of deploying to Cloudflare Pages. See the [project index](project-delidev.md).

## Users and Operators

- DeliDev users on desktop and mobile devices.
- Maintainers who develop, build and bundle the app.

## Interfaces and Contracts

- Entry point: `delidev_lib::run()` in `src/lib.rs`. `src/main.rs` calls it on desktop. On mobile, `tauri::mobile_entry_point` exports it to the Android and iOS host projects.
- CEF helper processes: CEF starts the same executable again as its renderer, GPU, network and utility processes with a `--type=` switch. `#[tauri_runtime_cef::cef_entry_point]` on `run()` makes those processes run the CEF helper and return before logging starts or the Tauri app is built. Every desktop entry point must keep this attribute.
- Window: one window with the label `main` and the title `DeliDev`.
- Frontend origin: the CEF runtime serves the bundled frontend from `http://tauri.localhost` on every desktop platform.
- Tauri commands:

| Command    | Arguments | Result    | Purpose                                               |
| ---------- | --------- | --------- | ----------------------------------------------------- |
| `app_info` | none      | `AppInfo` | Report the app version, webview runtime and platform. |

- `AppInfo` fields, serialized in camelCase: `version` (crate version), `runtime` (`WebviewRuntime`: `cef` or `wry`), `platform` (`PlatformFamily`: `desktop` or `mobile`) and `os` (`std::env::consts::OS`). `src/app-info.ts` mirrors these enums and the command name; change both sides together.
- In a plain browser (the Rsbuild development server opened without the Tauri shell) the frontend does not call commands and shows a browser notice instead.
- Capabilities: `capabilities/default.json` grants `core:default` to the `main` window only.
- Business flows with Delino services must use Connect RPC through `@connectrpc/connect-query` and React Query, not new Tauri commands.

## Storage

The scaffold stores no app data. CEF keeps its Chromium profile (cookies, cache and persisted permission decisions) in the runtime's default cache directory under the user cache directory. The CEF binary distribution is cached per machine outside the repository: `~/.cache/tauri-cef` on Linux, `~/Library/Caches/tauri-cef` on macOS and `%LOCALAPPDATA%\tauri-cef` on Windows, or `CEF_PATH` when it is set. App icons are below the Git LFS threshold of 512 KiB.

## Security

- The content security policy is not set (`app.security.csp` is `null`) in the scaffold. Define and validate a policy against the CEF IPC transport before the app loads remote content or user content.
- CEF sandboxing uses the runtime default `SandboxPolicy::Auto`. macOS always sandboxes. Linux sandboxes with unprivileged user namespaces or the setuid `chrome-sandbox` helper that Debian and RPM packages install; AppImages depend on user namespaces. Windows currently runs unsandboxed with every policy.
- On a Linux host that allows neither (for example a container without user namespaces, or Ubuntu 23.10 and later where AppArmor restricts unprivileged user namespaces for unbundled binaries), Chromium stops at startup with `No usable sandbox!`. `SandboxPolicy::Auto` turns the sandbox off only for an AppImage on such a system. For local diagnosis only, a debug build accepts `--no-sandbox` on its command line; do not ship a build that turns the sandbox off.
- DevTools open with F12 or Ctrl+Shift+I. Release builds include DevTools because the crate enables the `devtools` feature of `tauri-runtime-cef`; decide the release DevTools policy before the first release.
- Do not commit signing certificates, provisioning profiles, keystores or store credentials.

## Logging

- `src/logging.rs` installs a `tracing` subscriber that writes structured events to stderr.
- `DELIDEV_LOG` holds `tracing` filter directives. The default is `info`, which is also used when the value is empty or not valid.
- ANSI color is on by default. A non-empty `NO_COLOR` turns it off.
- `run()` logs one `starting delidev` event with the version, runtime, platform and operating system.
- Android and iOS do not show the process stderr. A platform log sink for mobile is not part of the scaffold.
- Logs must not contain access tokens, ID tokens or payment details.

## Build and Test

Prerequisites:

- The [Tauri prerequisites](https://v3.tauri.app/start/prerequisites/) of the host operating system.
- Linux: `pkg-config` and `libgtk-4-dev`. Tauri and `tauri-runtime-cef` use GTK 4 with CEF; the app does not need `libgtk-3-dev` or `libwebkit2gtk-4.1-dev`. To run the app, the host must allow the Chromium sandbox (see [Security](#security)).
- macOS and Windows: CMake and Ninja, for the CEF C++ wrapper.
- About 1 GB of disk space for the CEF distribution, which the first build downloads.
- Android: Android SDK, NDK and the Rust Android targets. iOS: macOS with Xcode and the Rust iOS targets.

Run from the repository root:

- `pnpm install --frozen-lockfile`
- `pnpm --filter delidev dev`: frontend development server at `http://127.0.0.1:3200` without the native shell. The server fails when the port is in use.
- `pnpm --filter delidev tauri dev`: desktop app on CEF. The Tauri CLI starts `pnpm dev` and loads `http://127.0.0.1:3200`. On macOS the CLI runs the app from an `.app` bundle.
- `pnpm --filter delidev tauri build`: desktop bundles. The CLI ships `libcef`, its resources and the `en-US` locale with the app.
- `pnpm exec turbo run typecheck build --filter=delidev`: frontend type check and production build into `apps/delidev/dist/`.
- `cargo fmt --all --check`, `cargo clippy --workspace --all-targets -- -D warnings` and `cargo test`: Rust checks of the root workspace on the host desktop target.
- `pnpm --filter delidev tauri android init` and `pnpm --filter delidev tauri ios init`: generate the Android and iOS host projects under `src-tauri/gen/`. Then use `tauri android dev`, `tauri android build`, `tauri ios dev` and `tauri ios build`.

`tauri dev` and `tauri build` for desktop compile the Rust crate with CEF. Physical Android and iOS devices load the development server through the LAN address in `TAURI_DEV_HOST`, which the Tauri CLI sets; `rsbuild.config.ts` binds to that address instead of the loopback address, and `turbo.json` passes `TAURI_DEV_HOST` to the `dev` task. Remove `apps/delidev/dist/` after validation.

CI runs the Rust checks in the `rust` job of the [workflow contract](repository-workflow-contract.md#validation) on Ubuntu. CI does not run `tauri build`, mobile builds, the frontend type check or build, or the app. The project has no e2e suite yet. The Android and iOS host projects are not generated in the repository yet.

## Dependencies and Integrations

- npm: `@tauri-apps/api`, `react` and `react-dom`; development: `@tauri-apps/cli`, `@rsbuild/core`, `@rsbuild/plugin-react`, `@types/*` and `typescript`, pinned in `apps/delidev/package.json`.
- Cargo: `tauri`, `tauri-build`, `tauri-runtime-cef` (desktop targets), `tauri-runtime-wry` (mobile targets), `serde`, `tracing` and `tracing-subscriber`, and `serde_json` for tests, in `apps/delidev/src-tauri/Cargo.toml`. Root `Cargo.lock` records the resolved versions.
- CEF binary distribution, downloaded by the `cef` crate build script from the CEF build server on the first build.
- Tauri crates and packages use `Apache-2.0 OR MIT`. CEF uses a BSD license; the bundles ship its license files. Check bundle notices in the distribution checks of the first release.

## Change Triggers

- Update [project-delidev.md](project-delidev.md) when paths, component IDs, the runtime per target or the bundle identifier change.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when the Rust checks, Linux packages or CI selection for this app change.
- Update [repository-environment-contract.md](repository-environment-contract.md) when the development server port or host binding changes.
- Update `apps/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delidev.md](project-delidev.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [Tauri CEF runtime](https://v3.tauri.app/develop/cef/)
- [Rsbuild documentation](https://rsbuild.rs/)
