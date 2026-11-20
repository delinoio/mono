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

- When Go code in this domain changes, run `git ls-files -z -- '*.go' | xargs -0 --no-run-if-empty gofmt -l` from the repository root and confirm that it prints no path, then run `go vet ./...` and `go test ./...` from the repository root before finishing.

## Owning contracts

- [Repository defaults](../docs/repository-defaults.md)
- [Repository workflow](../docs/repository-workflow-contract.md)
- `cmds/ownsand-agent/` and `cmds/ownsand-guest/`: [project index](../docs/project-ownsand.md) and [device agent foundation contract](../docs/cmds-ownsand-agent-foundation.md)
- `cmds/ownsand/`: [project index](../docs/project-ownsand.md) and [CLI foundation contract](../docs/cmds-ownsand-cli-foundation.md)
- Register each new project and its domain contracts in [the documentation catalog](../docs/README.md) before implementation.
