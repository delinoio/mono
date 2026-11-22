# cmds working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `cmds/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md` and command-specific docs in `docs/project-*.md` plus relevant `docs/cmds-*.md` files.

- Keep repository and domain rules in the appropriate `AGENTS.md` files.

- Write all source and comments in English.

- Prefer enums or typed constants over free-form string values.

- Keep command boundaries explicit and documented.

- Keep configuration schemas documented and synchronized with implementation.

- Add enough structured logging for step-level debugging and failure diagnosis.

- Do not log secret values for sensitive workflows.

- Keep integration boundaries with `apps/`, `servers/`, and other domains explicit in docs.

- Avoid undocumented cross-domain coupling.

## Licensing

- `cmds/ownsand-agent/` and `cmds/ownsand-guest/` are licensed under FSL-1.1-ALv2 (their `LICENSE` files), not the repository-wide Apache-2.0 license. Use `SPDX-License-Identifier: FSL-1.1-ALv2` in their source headers.
- The rest of `cmds/`, including `cmds/ownsand/`, `cmds/internal/` and new command tools, uses Apache-2.0. Use `SPDX-License-Identifier: Apache-2.0` in its source headers.
- Apache-2.0 code must not import code from `cmds/ownsand-agent/` or `cmds/ownsand-guest/`. Put code that both licenses use in an Apache-2.0 directory.
- A new FSL-1.1-ALv2 directory, or a move of code between license boundaries, needs an explicit relicensing decision. Follow `docs/repository-license-contract.md`.

## Owning contracts

- [Repository defaults](../docs/repository-defaults.md)
- [Repository workflow](../docs/repository-workflow-contract.md)
- [Repository licensing](../docs/repository-license-contract.md)
- `cmds/ownsand-guest/` and `cmds/ownsand-agent/`: [project index](../docs/project-ownsand.md) and [device agent foundation contract](../docs/cmds-ownsand-agent-foundation.md)
- `cmds/ownsand/`: [project index](../docs/project-ownsand.md) and [CLI foundation contract](../docs/cmds-ownsand-cli-foundation.md)
- `cmds/tide/` (`planned`): [project index](../docs/project-tide.md) and [CLI foundation contract](../docs/cmds-tide-cli-foundation.md)
- Register each new project and its domain contracts in [the documentation catalog](../docs/README.md) before implementation.
