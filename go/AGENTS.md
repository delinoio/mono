# go working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `go/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md`, the [public Go package rules](../docs/repository-workflow-contract.md#public-go-packages) and the owning project/domain contracts.
- Put only Go packages that external users import in this directory. Keep repository-only Go code in its owning project directory, and keep generated protobuf Go bindings under `protos/`.
- Treat import paths and exported identifiers as a public interface. Mark a change that moves, renames or removes them as breaking.

## Owning contracts

- [Repository workflow](../docs/repository-workflow-contract.md)
- `go/uuidv7/`: [repository workflow](../docs/repository-workflow-contract.md#public-go-packages)
- `go/ownsand/`: [project index](../docs/project-ownsand.md) and [SDK foundation contract](../docs/packages-ownsand-sdk-foundation.md)
- `go/tide/` (`planned`): [project index](../docs/project-tide.md) and [SDK foundation contract](../docs/packages-tide-sdk-foundation.md)
- Register each new package tree in its project index and domain contract before implementation.
