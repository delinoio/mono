# Project: delidev

## Goal

Provide DeliDev as one native app for desktop (Linux, macOS and Windows) and mobile (Android and iOS) devices. Desktop builds render the app with the same Chromium version on every operating system, so that web content behaves the same everywhere and the app can use Chromium features such as the DevTools Protocol, the process sandbox and command-line switches. Mobile builds share the same frontend and Rust core.

## Project ID

`delidev`

## Domain Ownership Map

- `apps/delidev/`: workspace package `delidev`, the Rsbuild React frontend and the Tauri CLI scripts.
- `apps/delidev/src-tauri/`: Rust crate `delidev` (library `delidev_lib`), the Tauri app shell. It is a member of the root Cargo workspace.

The project has one deployable, the native app, with one bundle per target platform. The Rust crate lives inside the app path because the Tauri CLI requires the `src-tauri` layout next to the frontend; it is not a shared crate under `crates/`. The project has no `cmds`, `servers`, `crates`, `protos` or `packages` paths.

## Domain Contract Documents

- [apps-delidev-foundation.md](apps-delidev-foundation.md)

## Cross-Domain Invariants

- Component IDs:

| Component ID       | Path                      | Purpose                                          |
| ------------------ | ------------------------- | ------------------------------------------------ |
| `delidev-frontend` | `apps/delidev/`           | Web frontend that the webview loads.             |
| `delidev-shell`    | `apps/delidev/src-tauri/` | Tauri app shell, commands and runtime selection. |

- Webview runtime per target, as the stable `WebviewRuntime` values that the `app_info` command returns:

| Target family | Operating systems     | `WebviewRuntime` | Runtime crate       |
| ------------- | --------------------- | ---------------- | ------------------- |
| `desktop`     | Linux, macOS, Windows | `cef`            | `tauri-runtime-cef` |
| `mobile`      | Android, iOS          | `wry`            | `tauri-runtime-wry` |

The Chromium Embedded Framework (CEF) has no Android or iOS port, so mobile targets use the system webview. Target-specific Cargo dependencies select the runtime crate; `tauri-build` and the Tauri CLI detect CEF from the same manifest per target.

- The app uses Tauri 3, which is in alpha. CEF support exists only in Tauri 3. Tauri crates and npm packages are pinned to exact alpha versions because alpha releases can break APIs.
- Bundle identifier: `io.delino.delidev`. Product name: `DeliDev`.
- Business flows with Delino services use Connect RPC, not Tauri-specific bindings. Tauri commands are limited to native capabilities of the device and the app shell.
- Authentication and account management follow the [authentication contract](repository-auth-contract.md). Billing follows the [billing contract](repository-billing-contract.md). The scaffold has no sign-in or billing flow yet.
- Deviation from repository defaults: the frontend uses Rsbuild instead of Rspress because it is an application. It is not a static site and does not deploy to Cloudflare Pages; it ships inside the native bundles.
- Changesets ignores `delidev` until `changeset version` can update the Cargo and Tauri versions together with the npm manifest; see the [deployment workflow](repository-workflow-contract.md#release-selection-with-changesets).
- Open decisions: release signing, notarization, store distribution, auto-update, the Android and iOS native project generation (`gen/android`, `gen/apple`) and the content security policy. Record each decision in the foundation contract before the implementation that depends on it.

## Change Policy

- Update [apps-delidev-foundation.md](apps-delidev-foundation.md) and this index together when paths, component IDs, the runtime per target or the bundle identifier change.
- Update the [workflow contract](repository-workflow-contract.md) when the Rust validation, Linux build packages or the Tauri toolchain change.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update `apps/AGENTS.md` only when development procedures, directory ownership or development rules change; follow the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts.

## References

- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [project-template.md](project-template.md)
- [Tauri CEF runtime](https://v3.tauri.app/develop/cef/)
