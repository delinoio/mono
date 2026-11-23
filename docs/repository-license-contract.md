# Repository License Contract

## Ownership

Delino-owned source and future distributions use Apache-2.0, except the FSL-1.1-ALv2 directories described below. Root `LICENSE` contains the complete terms and `NOTICE` identifies the copyright holder. New repository-owned manifests must declare Apache-2.0, or `FSL-1.1-ALv2` in an FSL-1.1-ALv2 directory.

## FSL-1.1-ALv2 directories

Source in these directories uses the Functional Source License, Version 1.1, ALv2 Future License (`FSL-1.1-ALv2`):

| Directory             | Contents             | License file                 |
| --------------------- | -------------------- | ---------------------------- |
| `servers/`            | Backend services     | `servers/LICENSE`            |
| `cmds/ownsand-agent/` | OwnSand device agent | `cmds/ownsand-agent/LICENSE` |
| `cmds/ownsand-guest/` | OwnSand guest helper | `cmds/ownsand-guest/LICENSE` |

The license file of each directory contains the complete terms and overrides the root Apache-2.0 license for that directory and its descendants. Each version converts to Apache-2.0 on the second anniversary of the date it is made available. Source headers and manifests in these directories declare `FSL-1.1-ALv2`. Keep the license file text unchanged except for the notice. All license files in the table have the same text; change them in the same change set.

The rest of `cmds/` stays Apache-2.0. This includes the OwnSand CLI (`cmds/ownsand/`), `cmds/internal/` and new command tools, unless this contract adds their directory to the table. The OwnSand CLI is a client of the public API and the Apache-2.0 SDKs, and package repositories that accept only OSI-approved licenses can distribute it.

Root `LICENSE-FSL` exists so that the repository license summary on GitHub lists both licenses. GitHub detects license files only at the repository root. `LICENSE-FSL` starts with a scope statement that limits its terms to the directories in the table, followed by the exact text of their license files. Change it in the same change set as those files. The root `README.md` must state the license of each directory before setup instructions.

## Code sharing across licenses

Apache-2.0 code must not import FSL-1.1-ALv2 code. Code in FSL-1.1-ALv2 directories can import Apache-2.0 code. Code that Apache-2.0 apps, packages, crates or command tools consume must live outside the FSL-1.1-ALv2 directories under Apache-2.0. Project indexes can add stricter import rules between FSL-1.1-ALv2 directories, for example between deployables. Moving code between an FSL-1.1-ALv2 directory and an Apache-2.0 directory changes its license and requires an explicit relicensing decision.

## Imported material

Imported agent skills retain their upstream licenses, attribution and notices. A repository-wide default does not relicense imported material. Keep the upstream `LICENSE` file in each imported skill directory. When upstream publishes none, keep a `NOTICE` file in that directory that names the source repository, skill path and commit as recorded in `skills-lock.json` and cites the upstream license declaration, without an invented copyright holder, year or license text; the `NOTICE` file does not change the upstream license. Keep original licenses and copyright notices for any future imported source, fonts and assets. Follow `repository-workflow-contract.md` for the import procedure.

## Distribution

Future distributions must include the complete applicable license terms and notices. A server image that the deployment workflow builds is a distribution and carries `servers/LICENSE`, `GO-LICENSE` (the Go toolchain license for the statically linked standard library and runtime) and `THIRD-PARTY-LICENSES.txt` (the license files of every third-party Go module that the production command package links) in `/usr/share/licenses/<service>/` ([deployment workflow](repository-workflow-contract.md#deployment-workflow)). A release of the OwnSand device agent carries `cmds/ownsand-agent/LICENSE`, a release of the OwnSand CLI carries the root `LICENSE` and `NOTICE`, and every binary release also carries `GO-LICENSE` (the Go toolchain license for the statically linked standard library and runtime) and `THIRD-PARTY-LICENSES.txt` (the license files of every linked Go module) as release assets next to the executables ([binary releases](repository-workflow-contract.md#binary-releases)); a guest image that contains the OwnSand guest helper carries `cmds/ownsand-guest/LICENSE`. Hosting a static site (for example on Cloudflare Pages) is not a distribution: the uploaded site does not need the root `LICENSE` and `NOTICE`. Validate package contents in the owning project's distribution checks. Apart from the server images and the GitHub Releases of the agent and the CLI that the deployment workflow publishes, this repository publishes no runtime packages or release artifacts yet.
