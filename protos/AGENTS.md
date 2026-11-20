# protos working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `protos/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md` and the owning project/domain contracts.

- Write schemas and comments in English.
- Regenerate and commit the Go bindings with `scripts/protos/generate.sh` in the same change as every schema edit; CI runs it with `--check`.

## Owning contracts

- [Repository defaults](../docs/repository-defaults.md)
- [Repository workflow](../docs/repository-workflow-contract.md)
- `protos/delino/ownsand/`: [project index](../docs/project-ownsand.md) and [API contract](../docs/protos-ownsand-api-contract.md)
- Register each new project and its domain contracts in [the documentation catalog](../docs/README.md) before implementation.
