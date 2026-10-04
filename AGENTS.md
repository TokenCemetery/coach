# AGENTS.md

## Repository Behavior

- Repo is source of truth. Verify memory and prior notes against it before acting.
- Limit to minimum. No refactor/reformat/improve unrelated code without approval.
- Update project-scoped documents in the same change if behavior they describe is affected.
- Final response must state: what changed, what verification ran, and any residual risk.

## Track Work in GitHub Issues

For bugs, future problems, or features: open GitHub issue (not chat/local files). Issues preserve history and progress.

```console
gh issue list --state all --search "keyword"   # look for an existing issue first
gh issue create --title "..." --body-file body.md --label "bug,P2"
```

- Check open and closed issues first. If missing, open before fixing — problem record is as valuable as the fix.
- Exception: fix uncommitted defects without an issue (it's part of the task, not history).
- Title states what is wrong or what should exist. Body gives the steps to reproduce, or what "done" looks like, plus the files involved.
- Use `--body-file` (inline double quotes execute backticks, deleting paths/identifiers).
- Label from `.github/workflows/labels.yml` (source of truth; only use declared labels).
- One type (`bug`, `feature`, `refactor`, `chore`, `docs`, `security`) + one priority (`P0`–`P3`). Add `needs-triage`, `needs-repro`, `blocked`, `declined`, `stale`, `help-wanted` as needed.
- All issues (open/closed) carry both labels, so queries by type/priority return full history.
- Comment findings and attempts to spare repeat investigation.
- Reference the issue from the commit or pull request that fixes it (`Fixes #12`), so it closes together with the change.

## Artifact Quality

- Every artifact must be complete, actionable, internally consistent, and specific enough to verify.
- No placeholders, TODOs, unsupported claims, or missing required sections unless the user requests a draft.
- Every section, example, and abstraction must contribute to the outcome. Remove anything that does not.
- Examples must be narrow, direct, complete, and consistent with actual repo interfaces.
- Introduce abstractions only when they reduce real complexity or follow an established pattern.
- No speculative features, unused extension points, or unrequested configurability.
- KISS: prefer the simplest complete solution. If an implementation grows beyond what the problem requires, simplify before finalizing.
- Self-review every non-trivial artifact for placeholders, contradictions, scope drift, and missing verification. Fix issues before presenting.
