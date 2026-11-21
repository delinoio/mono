# internal working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `internal/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md` and the [shared internal Go package rules](../docs/repository-workflow-contract.md#shared-internal-go-packages).
- Put only generic Go packages here that several projects or components of this repository use and that external users do not import. Keep project-specific code in its owning project directory, packages for external users under `go/`, and code that only servers share under `servers/internal/`.
- Packages here use Apache-2.0. They import only other packages in this directory, the standard library and external modules; they never import code from `servers/`, `go/` or a project directory.
- Add each new package to the package table of the owning contract in the same change. When a package changes, update every importer in the same change.
- Write all source and comments in English.
- When Go code in this directory changes, run `git ls-files -z -- '*.go' | xargs -0 --no-run-if-empty gofmt -l` from the repository root and confirm that it prints no path, then run `go vet ./...` and `go test ./...` from the repository root before finishing.

## Owning contracts

- [Repository workflow](../docs/repository-workflow-contract.md#shared-internal-go-packages)
- [Repository licensing](../docs/repository-license-contract.md)
