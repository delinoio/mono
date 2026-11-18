#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Release step of `.github/workflows/deploy.yml`. Run it in a checkout of the
# deployed revision after every selected deployment succeeded. It consumes the
# pending changesets, commits the versions and changelogs, tags the version
# commit and pushes `main` and the tags in one atomic push.
#
# Environment:
#   RELEASES             JSON array of {name, version} to tag (releases whose
#                        version advances).
#   SHA                  deployed revision; HEAD must be this commit.
#   GITHUB_STEP_SUMMARY  optional summary file.
#   DEPLOY_RELEASE_MAX_ATTEMPTS  optional push attempts (default 3).
set -euo pipefail

# Empty arrays under `set -u` and `mapfile` need Bash 4.4 or newer.
if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "::error::$0 needs Bash 4.4 or newer, not $BASH_VERSION."
  exit 2
fi

summary="${GITHUB_STEP_SUMMARY:-/dev/null}"
max_attempts="${DEPLOY_RELEASE_MAX_ATTEMPTS:-3}"

if [[ "$(git rev-parse HEAD)" != "$SHA" ]]; then
  echo "::error::HEAD is not the deployed revision $SHA."
  exit 1
fi

mapfile -t tags < <(jq -r '.[] | "\(.name)@\(.version)"' <<< "$RELEASES")

# Consume exactly the changesets that this run deployed. A version bump can
# change a `workspace:<version>` range, so refresh the lockfile in the same
# commit to keep frozen installs working.
pnpm exec changeset version
pnpm install --lockfile-only --no-frozen-lockfile --ignore-scripts
git add -A
if git diff --cached --quiet; then
  # Only changesets of ignored packages were pending; Changesets keeps them.
  echo "::notice::changeset version changed nothing; no version commit or tag."
  echo "No version commit: the pending changesets release no versioned package." >> "$summary"
  exit 0
fi

message_file="$(mktemp)"
trap 'rm -f "$message_file"' EXIT
{
  printf 'chore(release): version deployed packages\n\n'
  printf 'Deployed revision: %s\n' "$SHA"
  if (( ${#tags[@]} > 0 )); then
    printf '\n'
    printf -- '- %s\n' "${tags[@]}"
  fi
} > "$message_file"
git commit --quiet --file="$message_file"

# Tag the version commit, whose parent is the deployed revision, so each tag
# holds exactly the deployed tree plus the version files. The tags stay on
# this commit even when `main` gets a cherry-picked copy of it below.
version_commit="$(git rev-parse HEAD)"
tag_refspecs=()
for tag in "${tags[@]}"; do
  if git rev-parse --quiet --verify "refs/tags/$tag" > /dev/null; then
    echo "::error::Tag $tag already exists. Check the version in the pending changesets."
    exit 1
  fi
  git tag --annotate "$tag" --message "$tag deployed from $SHA" "$version_commit"
  tag_refspecs+=("refs/tags/$tag")
done

# Push `main` and every tag atomically, so a rejected push leaves neither on
# the remote. Only a moved `main` is retried: the version commit is
# cherry-picked onto the new `main` (changesets merged later stay pending).
# Any other rejection, such as a ruleset denial or an existing remote tag,
# fails at once.
for (( attempt = 1; ; attempt++ )); do
  if git push --atomic origin HEAD:refs/heads/main "${tag_refspecs[@]}"; then
    {
      echo "## Release"
      echo "Version commit on main: $(git rev-parse HEAD)"
      echo "Tagged version commit: $version_commit"
      if (( ${#tags[@]} > 0 )); then
        printf -- '- `%s`\n' "${tags[@]}"
      else
        echo "No tags: every release kept its version."
      fi
    } >> "$summary"
    exit 0
  fi
  git fetch --quiet origin main
  if git merge-base --is-ancestor origin/main HEAD; then
    echo "::error::The push was rejected although main did not move. Check the main ruleset and existing tags."
    exit 1
  fi
  if (( attempt >= max_attempts )); then
    echo "::error::main kept moving; the version commit and tags were not pushed after $max_attempts attempts."
    exit 1
  fi
  echo "::warning::main moved during the deployment; replaying the version commit (attempt $attempt of $max_attempts)."
  git reset --quiet --hard origin/main
  git cherry-pick "$version_commit"
done
