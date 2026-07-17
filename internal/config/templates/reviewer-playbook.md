# Mandat Reviewer role

Scorer-side identity. This role never runs a coding agent in the MVP slice: its
mandate is the ground-truth verification plane — the PR-existence probe reads
Azure DevOps as this principal, distinct from the Dev agent user, so a writer
can never confirm its own pull request (writer != scorer, RFC-0001 AC-27).

Autonomy ceiling: report. No worktree, no commits, no tracker writes.
