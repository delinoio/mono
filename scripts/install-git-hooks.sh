#!/usr/bin/env sh
set -eu

# Print the physical path of an existing directory and fail otherwise.
# Exit explicitly after a failed cd: "set -e" is ignored inside a command
# substitution that is an "if" condition under bash, so without the explicit
# exit the subshell would print the caller's directory with status 0 and two
# different missing paths would compare equal.
canonicalize_existing_directory() {
	(
		CDPATH=
		cd -- "$1" 2>/dev/null || exit 1
		pwd -P
	)
}

# Print a path that two spellings of the same hooks directory share, even when
# that directory does not exist yet. An existing directory resolves physically.
# A missing directory resolves through its parent, so "<git dir>/hooks/",
# "<git dir>/./hooks" and "<git dir>//hooks" all become "<physical git dir>/hooks".
# Fail when the parent is missing too; the caller then keeps the raw spelling, so
# an unrelated missing custom path still reaches Lefthook's own protection.
# The argument must be absolute, because dirname and basename of a relative
# path would resolve against the current directory.
comparable_hooks_path() {
	if resolved_hooks_path="$(canonicalize_existing_directory "$1")"; then
		printf '%s\n' "$resolved_hooks_path"
		return 0
	fi
	if resolved_hooks_parent="$(canonicalize_existing_directory "$(dirname -- "$1")")"; then
		printf '%s\n' "$resolved_hooks_parent/$(basename -- "$1")"
		return 0
	fi
	return 1
}

# Exit before Lefthook can overwrite "<hook>.old" with a non-Lefthook hook.
refuse_backup_overwrite() {
	hook_file="$canonical_hooks_path/$1"
	backup_file="$hook_file.old"
	if [ -f "$hook_file" ] && ! grep -q LEFTHOOK "$hook_file" &&
		{ [ -e "$backup_file" ] || [ -L "$backup_file" ]; }; then
		printf '%s\n' \
			"Refusing to install Lefthook: $hook_file is not managed by Lefthook and $backup_file already exists." \
			"Installing would overwrite $backup_file with $hook_file and lose the backup." \
			"Move or merge one of the two files, then run this script again." >&2
		exit 1
	fi
}

is_inside_work_tree="$(git rev-parse --is-inside-work-tree 2>/dev/null || true)"
if [ "$is_inside_work_tree" != "true" ]; then
	printf '%s\n' "Git metadata is unavailable; skipping Lefthook installation."
	exit 0
fi

repo_root="$(git rev-parse --show-toplevel)"

# An archive extracted below an unrelated work tree is inside that work tree
# without owning Git metadata; installing there would modify the other repository.
project_root="$(canonicalize_existing_directory "$(dirname -- "$0")/..")"
if [ "$(canonicalize_existing_directory "$repo_root")" != "$project_root" ]; then
	printf '%s\n' "Git metadata is unavailable; skipping Lefthook installation."
	exit 0
fi

git_common_dir="$(git rev-parse --path-format=absolute --git-common-dir)"
configured_hooks_path="$(git config --path --get core.hooksPath || true)"
shared_hooks_path="$git_common_dir/hooks"

if [ -z "$configured_hooks_path" ]; then
	exec lefthook install
fi

case "$configured_hooks_path" in
	/*|[A-Za-z]:/*) absolute_hooks_path="$configured_hooks_path" ;;
	*) absolute_hooks_path="$repo_root/$configured_hooks_path" ;;
esac

canonical_hooks_path="$absolute_hooks_path"
if normalized_hooks_path="$(comparable_hooks_path "$absolute_hooks_path")"; then
	canonical_hooks_path="$normalized_hooks_path"
fi

canonical_shared_hooks_path="$shared_hooks_path"
if normalized_shared_hooks_path="$(comparable_hooks_path "$shared_hooks_path")"; then
	canonical_shared_hooks_path="$normalized_shared_hooks_path"
fi

if [ "$canonical_hooks_path" = "$canonical_shared_hooks_path" ]; then
	# Lefthook rejects an existing core.hooksPath in linked worktrees even when it
	# already targets Git's shared hooks directory. Force bypasses that check
	# only when both paths resolve to that repository-owned directory; remove
	# this branch when Lefthook handles it.
	#
	# Force also disables Lefthook's guard for an existing "<hook>.old" backup:
	# Lefthook would rename a non-Lefthook hook over that backup and lose it.
	# Refuse before forcing when that would happen. A Lefthook-managed hook is
	# removed without touching its backup, and a missing hook or missing backup
	# is safe. Check every hook that lefthook.yml defines (only pre-commit today).
	refuse_backup_overwrite pre-commit
	exec lefthook install --force
fi

# Preserve Lefthook's protective failure for unrelated custom hook paths.
exec lefthook install
