---
name: setup-infisical-dev
description: Configure the Infisical `dev` environment so that the repository development servers start with the variables they need. Fill each value the agent can determine from the repository, and ask the user for each value that only the user can supply. Use only when a human explicitly invokes $setup-infisical-dev or asks to run Set Up Infisical Development Variables. Do not activate automatically for environment, configuration or Infisical tasks.
disable-model-invocation: true
---

# Set Up Infisical Development Variables

This skill fills the Infisical `dev` environment so that `pnpm dev` starts the development servers with their configuration. The agent sets each value that it can determine from the repository. It asks the user for each value that only the user can supply.

The owning contract is [Local development injection](../../../docs/repository-environment-contract.md#local-development-injection). Read that section before you start. When this skill and the contract do not agree, obey the contract.

Run this skill only when a human explicitly invokes it. Do not start it from task similarity, from another skill or from an automation.

Write all messages to the user in the language of the user's request.

## Safety rules

- Write only to the `dev` environment. Pass `--env=dev` explicitly on every command. Never read or write another environment.
- Never print, log or echo a variable value. Do not put a value in a chat message, a command line, a commit, a PR, an issue or a file in the repository. Show names only. When a command prints values (for example a secrets table), do not run it; use the names-only commands below.
- Pass each value to the CLI through a file (`NAME=@<file>` or `--file=<file>`), never as a literal command-line argument, because the shell history and the process list keep command-line arguments. Create the file with mode `0600` outside the repository's tracked paths (in a T3 Code worktree, in `.t3-tmp/`), and delete it immediately after the command.
- A folder holds only the variables of its own package. Never put a server credential in a frontend folder. A frontend folder holds only public `PUBLIC_` build values, because the build inlines them into the browser bundle.
- Do not define a name that the contract forbids in a development folder: `PORTLESS` and `PORTLESS_*`, a listen address or port (for example `ADDR` or `STUN_ADDR`), and the reserved names that the launcher skips (`HOME`, `PATH`, `XDG_*`, `LC_*` and the others in the contract). A `.env.example` file can list such a name for deployments; do not copy it.
- Shared values in the `dev` environment affect every maintainer. Do not overwrite or delete an existing value unless the user approves that specific change.
- Never commit `.infisical.yaml`, `.infisical.json` or a file that contains a value.

## 1. Check the prerequisites

Run these checks in the repository root of the current checkout (`git rev-parse --show-toplevel`). The launcher reads the project configuration only from there.

1. **CLI.** Run `infisical --version`. If the command is missing, tell the user to install the [Infisical CLI](https://infisical.com/docs/cli/overview), and stop until the user confirms the installation.
2. **Project configuration.** Look for `.infisical.yaml` or the legacy `.infisical.json` in the repository root.
   - If the file is missing in a linked worktree, find the main checkout with `git worktree list --porcelain` (the first `worktree` entry). If the main checkout has the file, copy it to the root of the current checkout and tell the user that you copied it.
   - If no checkout has the file, ask the user to run `infisical init` in the repository root and to select the development project. The command is interactive; in Claude Code the user can run it with `! infisical init`.
   - Read the file. If it sets `general.domain` or `domain`, show the host to the user and continue only when the user confirms that the team operates that instance. The CLI sends its credentials to that host.
3. **Login.** Run a read-only command that needs a session, for example the names-only export in [step 3](#3-read-the-current-state) for one folder. If it fails because no session exists or the session expired, ask the user to run `infisical login` (interactive; `! infisical login` in Claude Code), then retry.
4. **Command family.** CLI versions use different command names. Run `infisical secrets-manager --help`.
   - If it succeeds, use the current commands: `infisical secrets-manager secrets create`, `secrets update`, `folders get` and `folders create <name>`.
   - If it fails, use the legacy commands: `infisical secrets set`, `infisical secrets folders get` and `infisical secrets folders create --name=<name>`.
   - Before you use a command for the first time, read its `--help` output and confirm the flags (`--env`, `--path`, `--type`, `--file`). Do not guess a flag.

## 2. Make the variable inventory

1. Read the package table in [Local development injection](../../../docs/repository-environment-contract.md#local-development-injection). It gives each package that uses the launcher and its Infisical folder (the package directory with a leading `/`). Configure every package in that table, unless the user named only some packages.
2. For each package, find the variables that its development server reads. Read these sources:
   - The configuration section of the package's owning contract. Find the contract through [the documentation catalog](../../../docs/README.md).
   - The package's `.env.example` file.
   - The development launcher of the package (for example `scripts/dev.mjs` of a server). It can apply a development default when a variable is unset, or skip the server when a required variable is unset.
   - The configuration validation of the package (for example the config loader of a server or `rsbuild.config.ts` of a frontend).
3. Give each variable exactly one disposition:

   | Disposition | When                                                                                                                                                                                                                                            | Action                                                                                                   |
   | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
   | `unset`     | The code default or the launcher default is correct for development (for example the `CORS_ORIGINS` default of a server launcher), or the contract forbids the name in a development folder.                                                    | Do not set it. Record the reason.                                                                        |
   | `derived`   | The repository documents the development value, for example the loopback origin of another development server in [Process boundaries](../../../docs/repository-environment-contract.md#process-boundaries) (a frontend `PUBLIC_API_ORIGIN`). | Set the documented value. Cite the source.                                                               |
   | `generated` | The value is a random key that the contract lets you create, and development needs it.                                                                                                                                                          | Generate it locally (for example with `openssl rand`) into a `0600` file in the format that the contract gives. |
   | `user`      | Only the user can supply it: an external account or tenant value (for example a Logto endpoint or application ID), a database URL, or a third-party credential.                                                                                 | Ask the user, as [step 4](#4-ask-the-user-for-missing-values) describes.                                 |
   | `optional`  | It enables a feature that development does not need (for example object storage for exports).                                                                                                                                                 | Ask the user whether to enable the feature. Without a yes, leave it unset.                               |

   Do not copy a production value from a `.env.example` file or a contract into the `dev` environment. A `.env.example` value is a placeholder or a deployment default, not a development value.

## 3. Read the current state

For each folder, read which names already exist, without their values:

```sh
infisical export --env=dev --path="<folder>" --format=json --silent \
  | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const v=JSON.parse(s);console.log((Array.isArray(v)?v.map(e=>e.key):Object.keys(v)).sort().join("\n"))})'
```

The command fails when the folder does not exist; check the folder with the `folders get` command of your command family. The export includes your personal overrides, so a name can be your own override and not a shared value.

Compare the existing names with the inventory:

- A required name that exists: keep it. Do not ask for it again unless the user reports that its value is wrong.
- A name that the contract forbids in the folder, or a server credential in a frontend folder: report it to the user and ask whether to delete it.
- A name that the package does not read: report it. Do not delete it without the user's approval.

## 4. Ask the user for missing values

Ask for all `user` and `optional` values in one batch, not one at a time. For each value, tell the user:

- the name, the folder and what the server uses it for;
- whether development needs it, and what happens without it (for example: without `DATABASE_URL`, the `ownsand-api` launcher skips the server and `pnpm dev` starts the other servers);
- where to find it (for example the Logto console of the development tenant), and the format that the contract requires;
- whether it is a secret.

Also ask whether each value is the same for every maintainer (store it as a shared value) or differs for each developer (store it as a personal override with `--type=personal`). A team tenant value is usually shared. A URL of a database on the user's own machine is usually personal. When the answer is not clear, ask; do not decide alone.

Collect values safely:

- For a secret value, use the harness's secret input tool when one exists, for example T3 Code `request_secret` with `delivery: "file"`, and pass the file it returns to the CLI as `NAME=@<file>`. Without such a tool, give the user a file path (mode `0600`, outside the tracked paths) and ask the user to write the value into it. Do not ask the user to paste a secret into the chat.
- A public value (for example a `PUBLIC_` value or a Logto application ID) can come in the chat. Still pass it to the CLI through a file.

If the user skips a value, leave it unset and record what does not work without it.

## 5. Confirm and write

1. Show the plan to the user before you write anything. List each folder, and for each variable: the name, the disposition, the source (contract, default, generated or user) and `shared` or `personal`. Mark each change to an existing value. Do not show values.
2. Write only after the user approves the plan.
3. Create each missing folder with the `folders create` command of your command family, with `--env=dev`. Current CLI versions create missing parent folders; with a legacy CLI, create `/servers` or `/apps` first when it is missing.
4. For each new name, run the `create` command (legacy: `secrets set`) with `--env=dev --path="<folder>"`, the `--type` from the plan and `NAME=@<file>`. For an approved change to an existing name, use the `update` command (legacy: `secrets set`).
5. Delete each value file immediately after its command, also when the command fails.

## 6. Verify

1. Run the names-only export from [step 3](#3-read-the-current-state) for each folder, and compare the names with the plan.
2. Start each configured package's development server once with `pnpm --filter <package> dev`, and stop it after it reports that it is ready or after it fails. Read the `dev_env_infisical` record of the launcher: it gives the mode and the count of injected variables. Confirm that the server started (for example a server answers `GET /healthz` on its loopback port, and a frontend development server starts without a configuration validation error). Do not print the server's environment.
3. If a server fails because of its configuration, find the variable, fix it through steps 4 and 5, and verify again. If it fails for another reason (for example no Postgres server runs at the URL that the user gave), report the cause and do not change unrelated code or configuration.
4. Stop every process that you started, and delete every value file that you created.

## 7. Report

Report in the user's language:

- for each folder: the names that you set, with their disposition, source and `shared` or `personal`, and the names that already existed;
- each name that you left unset, and why;
- each value that the user skipped, and what does not work without it;
- the result of each development server start;
- any action that only the user can do (for example start a local Postgres server or register a Logto application).

Do not include any value in the report.
