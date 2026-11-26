---
name: batch-mode
description: Use when a human explicitly invokes $batch-mode or requests Batch Mode, in which many issues are fixed in one batch run. List the Linear issues that the batch must fix and apply the batch CI constraints. Do not activate automatically for batch work or CI tasks.
disable-model-invocation: true
---

# Batch Mode

Batch Mode is a run in which many issues are fixed in one batch. This skill tells the batch which issues to fix and adds constraints for the batch work.

Apply these additional instructions to the current task only when a human explicitly invokes this skill.

## Batch Issue Selection

The batch contains every Linear issue that matches both filters:

- Team: `DELI` (Mono).
- Status: `Todo`.

Do not add issues from other teams or statuses, such as `Backlog`, `In Progress` or `In Review`, to the batch.

1. Query Linear with `list_issues` (Linear MCP server), `team: "DELI"` and `state: "Todo"`. Request the `id`, `title`, `priority`, `labels`, `assignee` and `url` fields.
2. When `hasNextPage` is `true`, fetch the next page with the returned `cursor` until all pages are read. Do not assume that the first page is complete.
3. If the `Todo` status cannot be resolved, run `list_issue_statuses` for the `DELI` team and use the status named `Todo`. Do not substitute another status.
4. Report the batch as a list. For each issue, give the identifier (for example `DELI-123`), the title, the priority and the URL. If no issue matches, report that the batch is empty.

This list is the work list of the batch. Fix every issue in the list. Do not fix issues that are not in the list as part of the batch.

Use the issue identifiers in this list for the `Closes DELI-<number>` and `Refs DELI-<number>` lines of each PR. Follow [the issue tracker contract](../../../docs/repository-issue-tracker-contract.md).

## Dependency Constraints

- Do not add new dependencies in the batch work. This includes packages, crates, Go modules and GitHub Actions.
- Use only the dependencies that the repository already has.
- If an issue cannot be fixed without a new dependency, do not fix it as part of the batch. Handle it as described in [Unprocessable Issues](#unprocessable-issues).

## CI Constraints

- Do not start manual CI or trigger `workflow_dispatch`.
- Use the existing PR's automatic CI.
- Retrying a failed CI run is allowed.
- Never modify `.github/workflows/CI.yml`.
- If an issue cannot be fixed without a manual CI run or a change to `.github/workflows/CI.yml`, do not fix it as part of the batch. Handle it as described in [Unprocessable Issues](#unprocessable-issues).

## Unprocessable Issues

An issue is unprocessable when the rules of this skill do not permit the batch to fix it. Examples are an issue that needs a new dependency, a manual CI run or a change to `.github/workflows/CI.yml`.

For each unprocessable issue in the batch work list:

1. Do not implement a fix and do not create a PR for the issue.
2. Use `list_comments` to look for an existing comment on the issue that states which rule prevents the fix and what the issue needs before it can be fixed. Check further pages as needed. Reuse a comment that describes the current blocker, including when retrying a failed move. If the lookup is incomplete and no matching comment is confirmed, report an incomplete handoff and leave the issue in `Todo`.
3. If no matching comment exists, use `save_comment` to add one that names the rule and what the issue needs. Confirm that the response identifies the saved comment. If the write fails or its response is uncertain, use `list_comments` to check whether the comment exists. If you still cannot confirm the comment, report an incomplete handoff and leave the issue in `Todo` so a later batch can retry.
4. After confirming the comment, move the issue to `Backlog` with `save_issue` (Linear MCP server). If the `Backlog` status cannot be resolved, run `list_issue_statuses` for the `DELI` team and use the status named `Backlog`. Do not substitute another status.
5. If the move fails or its response is uncertain, use `get_issue` to check the current status. Report an incomplete handoff unless you confirm `Backlog`. On retry, reuse the confirmed comment for the same blocker.
6. In the batch report, list the issue with its identifier, its title, its URL and the blocking reason. Mark incomplete handoffs and state which step could not be confirmed.

Move an issue to `Backlog` only when a rule of this skill prevents the fix. Do not move an issue because it is difficult, large or slow to fix.

This skill gives the batch issue list and adds instructions only. It does not fix issues or create PRs by itself; the batch work does that for each issue in the list.
