#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Recovery step of the `release` job of `.github/workflows/deploy.yml`. Run it
# in a checkout of the deployed revision on a rerun (`GITHUB_RUN_ATTEMPT` > 1)
# before the stale-plan check.
#
# `deploy-release.sh` pushes `main` and the tags in one atomic push. The push
# can land while the job still fails (the connection dropped before the
# response, or a later step failed). A rerun of the failed job then fails the
# stale-plan check, because the planned changesets are gone from `main`, and
# `publish-binaries` never starts although the tags exist.
#
# This script proves that the release of this run already landed, without any
# write to the remote and without a new commit or tag, and sets the step
# output `recovered=true` so that the job skips the stale-plan check and the
# push and concludes with success. In every other case it sets
# `recovered=false` and exits 0, so that the unchanged checks of the job
# decide. Recovery needs all of the following proofs:
#
# - exactly one commit on `main` after the deployed revision records
#   `Deployed revision: <SHA>` and `Deployment run: <run ID>` (the run ID of
#   all attempts is the same), has one parent and is the version commit;
# - that commit deletes every planned changeset with exactly the planned blob,
#   and no planned changeset exists on the tip of `main`;
# - every planned `<name>@<version>` is an annotated tag whose message is
#   `<name>@<version> deployed from <SHA>` and that peels to one commit
#   whose parent is the deployed revision and whose message is the message of
#   the version commit on `main`;
# - the version files of that commit (`package.json` with the planned name and
#   version) and its list of changed paths match the commit on `main`. When
#   `main` moved during the deployment, `deploy-release.sh` pushed a
#   cherry-picked copy of the tagged commit, so the commit IDs can differ;
# - no run of the deploy workflow started after this run (a superseding
#   deployment), checked by `check-pending-changesets.sh` with `RUNS_ONLY`.
#
# Environment:
#   RELEASES             JSON array of {name, version} planned tags.
#   CHANGESET_BLOBS      JSON object of planned changeset id to blob SHA.
#   SHA                  deployed revision; HEAD must be this commit.
#   GITHUB_RUN_ID        run ID of this run (kept by reruns).
#   GITHUB_RUN_ATTEMPT   attempt of this run; recovery needs more than 1.
#   GITHUB_RUN_NUMBER, REPOSITORY, WORKFLOW, GH_TOKEN
#                        for the newer-run check of
#                        `check-pending-changesets.sh`.
#   GITHUB_OUTPUT        optional step output file.
#   GITHUB_STEP_SUMMARY  optional summary file.
set -euo pipefail

# Empty arrays under `set -u` and `mapfile` need Bash 4.4 or newer.
if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
  echo "::error::$0 needs Bash 4.4 or newer, not $BASH_VERSION."
  exit 2
fi

summary="${GITHUB_STEP_SUMMARY:-/dev/null}"
output="${GITHUB_OUTPUT:-/dev/null}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
main_ref="refs/remotes/origin/main"

not_proven() {
  echo "::notice::No landed release to recover: $1"
  echo "recovered=false" >> "$output"
  exit 0
}

if [[ ! "${GITHUB_RUN_ATTEMPT:-}" =~ ^[0-9]+$ ]] || (( GITHUB_RUN_ATTEMPT < 2 )); then
  not_proven "this is not a rerun (attempt ${GITHUB_RUN_ATTEMPT:-<empty>})."
fi
if [[ ! "${GITHUB_RUN_ID:-}" =~ ^[0-9]+$ ]]; then
  not_proven "GITHUB_RUN_ID is not a number: ${GITHUB_RUN_ID:-<empty>}."
fi
if [[ ! "${SHA:-}" =~ ^[0-9a-f]{40}$ ]]; then
  not_proven "SHA is not a full commit SHA: ${SHA:-<empty>}."
fi
if ! jq -es 'length == 1 and (.[0] | type == "array" and all(.[]; type == "object" and (.name | type == "string" and length > 0) and (.version | type == "string" and length > 0)))' \
  <<< "${RELEASES:-}" > /dev/null 2>&1; then
  not_proven "RELEASES is not a JSON list of releases."
fi
if ! jq -es 'length == 1 and (.[0] | type == "object" and length > 0 and all(.[]; type == "string" and test("^[0-9a-f]{40}$")))' \
  <<< "${CHANGESET_BLOBS:-}" > /dev/null 2>&1; then
  not_proven "CHANGESET_BLOBS is not a nonempty map of changeset blob SHAs."
fi
if [[ "$(git rev-parse HEAD)" != "$SHA" ]]; then
  not_proven "HEAD is not the deployed revision $SHA."
fi

mapfile -t tags < <(jq -r '.[] | "\(.name)@\(.version)"' <<< "$RELEASES")
mapfile -t release_lines < <(jq -r '.[] | [.name, .version] | @tsv' <<< "$RELEASES")
mapfile -t changeset_ids < <(jq -r 'keys[]' <<< "$CHANGESET_BLOBS")

# Read-only proof: only fetches into the local checkout. A failing fetch (for
# example a tag that is not on the remote) proves nothing.
if ! git fetch --quiet origin "+refs/heads/main:$main_ref"; then
  not_proven "could not fetch main."
fi
for tag in "${tags[@]}"; do
  if ! git fetch --quiet origin "refs/tags/$tag:refs/tags/$tag" 2> /dev/null; then
    not_proven "tag $tag is missing on the remote or differs from the local tag."
  fi
done

# The version commit on `main`: one commit after the deployed revision that
# records this revision and this run.
candidates=()
while IFS= read -r commit; do
  [[ -n "$commit" ]] || continue
  body="$(git show --no-patch --format=%B "$commit")"
  if [[ "$(head -n 1 <<< "$body")" == "chore(release): version deployed packages" ]] \
    && grep -qxF "Deployed revision: $SHA" <<< "$body" \
    && grep -qxF "Deployment run: $GITHUB_RUN_ID" <<< "$body"; then
    candidates+=("$commit")
  fi
done < <(git rev-list "$SHA..$main_ref")
if (( ${#candidates[@]} != 1 )); then
  not_proven "main has ${#candidates[@]} version commits of revision $SHA and run $GITHUB_RUN_ID, not exactly one."
fi
main_commit="${candidates[0]}"
read -r -a parents <<< "$(git rev-list --parents -n 1 "$main_commit")"
if (( ${#parents[@]} != 2 )); then
  not_proven "the version commit $main_commit on main is a merge commit."
fi
main_parent="${parents[1]}"
if ! git merge-base --is-ancestor "$SHA" "$main_parent"; then
  not_proven "the parent of the version commit $main_commit does not contain $SHA."
fi

# Every planned changeset is deleted by the version commit with exactly the
# planned blob and is gone from the tip of `main`.
declare -A deleted=()
while IFS=$'\t' read -r meta path; do
  [[ -n "$meta" ]] || continue
  read -r _ _ old_blob _ status <<< "$meta"
  if [[ "$status" == D ]]; then
    deleted["$path"]="$old_blob"
  fi
done < <(git diff-tree -r --no-renames --no-commit-id --raw --no-abbrev "$main_parent" "$main_commit")
for id in "${changeset_ids[@]}"; do
  planned="$(jq -r --arg id "$id" '.[$id]' <<< "$CHANGESET_BLOBS")"
  if [[ "${deleted[.changeset/$id.md]:-}" != "$planned" ]]; then
    not_proven ".changeset/$id.md is not consumed by the version commit $main_commit with the planned blob."
  fi
  if git cat-file -e "$main_ref:.changeset/$id.md" 2> /dev/null; then
    not_proven ".changeset/$id.md exists again on the tip of main."
  fi
done

# The files that a commit changed, as `<status> <path>` lines.
changed_files() {
  git diff-tree -r --no-renames --no-commit-id --name-status "$1" "$2" | sort
}
# Succeeds when a changed `package.json` of the commit has the name and
# version of the release. The arguments are the commit, the changed files and
# the release name and version.
has_version() {
  local commit="$1" files="$2" name="$3" version="$4" status path
  while IFS=$'\t' read -r status path; do
    [[ "$path" == package.json || "$path" == */package.json ]] || continue
    if [[ "$(git show "$commit:$path" 2> /dev/null | jq -r '[.name, .version] | @tsv' 2> /dev/null || true)" == "$name"$'\t'"$version" ]]; then
      return 0
    fi
  done <<< "$files"
  return 1
}

main_files="$(changed_files "$main_parent" "$main_commit")"
tagged_commit=""
for tag in "${tags[@]}"; do
  # `%(*objectname)` is the commit that the annotated tag points to.
  IFS=' ' read -r object_type object_name subject < <(git for-each-ref --format='%(objecttype) %(*objectname) %(contents:subject)' "refs/tags/$tag") || true
  if [[ "$object_type" != tag || "$subject" != "$tag deployed from $SHA" ]]; then
    not_proven "$tag is not an annotated tag of the deployment of $SHA."
  fi
  if [[ "$(git cat-file -t "$object_name")" != commit ]]; then
    not_proven "$tag does not point to a commit."
  fi
  if [[ -z "$tagged_commit" ]]; then
    tagged_commit="$object_name"
  elif [[ "$object_name" != "$tagged_commit" ]]; then
    not_proven "the planned tags point to different commits."
  fi
done
if [[ -n "$tagged_commit" ]]; then
  read -r -a tagged_parents <<< "$(git rev-list --parents -n 1 "$tagged_commit")"
  if (( ${#tagged_parents[@]} != 2 )) || [[ "${tagged_parents[1]}" != "$SHA" ]]; then
    not_proven "the tagged commit $tagged_commit does not have the deployed revision $SHA as its only parent."
  fi
  if [[ "$(git show --no-patch --format=%B "$tagged_commit")" != "$(git show --no-patch --format=%B "$main_commit")" ]]; then
    not_proven "the tagged commit $tagged_commit and the version commit $main_commit on main have different messages."
  fi
  tagged_files="$(changed_files "$SHA" "$tagged_commit")"
  # A cherry-picked copy has other blobs when `main` changed a changelog
  # meanwhile, but it changes the same paths in the same way.
  if [[ "$tagged_files" != "$main_files" ]]; then
    not_proven "the version commit $main_commit on main and the tagged commit $tagged_commit change different files."
  fi
  for line in "${release_lines[@]}"; do
    IFS=$'\t' read -r name version <<< "$line"
    if ! has_version "$tagged_commit" "$tagged_files" "$name" "$version" \
      || ! has_version "$main_commit" "$main_files" "$name" "$version"; then
      not_proven "$name@$version is not the version of a changed package.json on both the tagged commit and main."
    fi
  done
fi

# A superseding deployment: a newer run may already have released other
# changesets or deployed a newer revision.
if ! runs_output="$(RUNS_ONLY=true bash "$script_dir/check-pending-changesets.sh" 2>&1)"; then
  echo "$runs_output"
  not_proven "a newer deployment run exists or the runs could not be checked."
fi
echo "$runs_output"

echo "::notice::The release of this run already landed: version commit $main_commit on main; nothing is pushed."
{
  echo "## Release"
  echo "Recovered: the version commit $main_commit on main and the planned tags of revision $SHA already landed in an earlier attempt of this run. Nothing was pushed."
  if [[ -n "$tagged_commit" ]]; then
    echo "Tagged version commit: $tagged_commit"
    printf -- '- `%s`\n' "${tags[@]}"
  else
    echo "No tags: every release kept its version."
  fi
} >> "$summary"
echo "recovered=true" >> "$output"
