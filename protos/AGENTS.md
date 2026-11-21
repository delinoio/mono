# protos working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `protos/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md` and the owning project/domain contracts.

- Write schemas and comments in English.
- After a schema change, run `scripts/protos.sh generate` from the repository root and commit the schemas with the generated Go bindings. `scripts/protos.sh check` must pass; CI runs it with the breaking-change check. Never edit generated `*.pb.go` or `*.connect.go` files by hand.

## Owning contracts

- [Repository defaults](../docs/repository-defaults.md)
- [Repository workflow](../docs/repository-workflow-contract.md)
- `protos/delino/ownsand/`: [project index](../docs/project-ownsand.md) and [API contract](../docs/protos-ownsand-api-contract.md)
- `protos/delino/accounts/`: [project index](../docs/project-delino-accounts.md) and [API contract](../docs/protos-delino-accounts-api-contract.md)
- `protos/delino/tide/` (`planned`): [project index](../docs/project-tide.md) and [API contract](../docs/protos-tide-api-contract.md)
- Register each new project and its domain contracts in [the documentation catalog](../docs/README.md) before implementation.
