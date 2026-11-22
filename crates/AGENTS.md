# crates working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `crates/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md` and each crate-specific project document.
- The root `Cargo.toml` defines the Rust workspace (resolver 3, shared package fields and explicit members). Add every new crate, including Tauri app crates under `apps/<app>/src-tauri/`, as an explicit workspace member in the same change that creates it. Do not add nested workspaces.
- Keep crate naming aligned with project IDs when possible.
- Document behavior contracts in project index docs and relevant crate-domain docs before large implementation changes.
- Planned crate paths must not be added as workspace members until the crate skeleton exists.
- Prefer minimal default features and keep optional capabilities opt-in for size-sensitive crates.
- Keep proc-macro crates and runtime crates separated by explicit crate boundaries.
- Test cleanup must remove only the fixture-owned directory, never the process-wide temporary directory or sibling fixtures.

## Owning contracts

- [Repository defaults](../docs/repository-defaults.md)
- [Repository workflow](../docs/repository-workflow-contract.md)
- Register each new project and its domain contracts in [the documentation catalog](../docs/README.md) before implementation.
