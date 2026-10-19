# Review and merge process

How a Bearing pull request is reviewed, how the review is shown on the PR,
and when it merges. [`AGENTS.md`](../AGENTS.md) points here; read both
before reviewing or merging.

## Reviewers

Each PR gets one or two reviewers, chosen from
[`.github/reviewers.yml`](../.github/reviewers.yml) by the paths it touches.
If `security` matches, it always takes one of the two slots. If more than
two domain reviewers match, the lead picks the two most relevant and says
why in the PR. `go` takes the second slot when only one domain reviewer
matches and the PR changes Go, and is the only reviewer when no domain
reviewer matches. A PR that matches no rule gets the file's `default`.
Reviewer definitions live in [`.claude/agents/`](../.claude/agents/).

## Findings

Every finding states severity, effort to fix now, how hard it is to change
later, and whether leaving it opens an important gap against the project's
goals. A defect that makes the change wrong, unsafe, or not do what it
claims (a bug, a missing check, a test that cannot fail) leaves an
important gap. Style and polish do not. Only findings that are hard to
change later or leave an important gap block; the rest become follow-up
issues. The lead files them as GitHub issues linked from the PR before
merging.

## Rounds

A round is one push answering the findings plus the reviewers' re-review.
Up to five rounds per PR. The first review is Round 1, so push k's
re-review is Round k+1 and the fifth push ends with Round 6. After the
fifth push's round ends with blocking findings open, the lead stops and
mentions @dhm116 (Doug, the maintainer) in a new comment: the round's own
comment is updated by editing, and an edit notifies no one.

## Review comment

Each round has one PR comment, posted and kept current with
[`tools/reviewcard`](../tools/reviewcard/main.go):

1. **The round starts.** Before the reviewers run, the lead posts the round
   with every reviewer `running`. The first post for a round hides the
   lead's earlier review comments on the PR (minimized as outdated, never
   deleted), so the PR shows only the current round.
2. **A reviewer finishes.** The lead runs it again with that reviewer's
   verdict (`approve`, `discuss` for Needs Discussion, or `changes` for
   Request Changes), each blocking finding in a few words, and its number of
   suggestions. This edits the same comment.
3. **The round ends.** The lead runs it once more with the findings tables
   as `-details`.

The comment is a picture of the round with the tables under it. The
picture has one card per reviewer, showing its verdict, its counts and its
blocking findings, and an overall verdict once every reviewer is done. It
comes in light and dark and switches with the reader's GitHub theme. Its
colors are the home page's "Today" trail: green for Approve, amber for
Needs Discussion, red for Request Changes, grey while a reviewer is
running. The image's alt text carries everything the cards show.

```sh
cat > /tmp/round.json <<'EOF'
{"round": 2, "reviewers": [
  {"name": "security", "state": "changes", "suggestions": 3,
   "blockers": ["Webhook body is read before the size cap"]},
  {"name": "go", "state": "running"}]}
EOF
go -C tools run ./reviewcard -pr 292 -round /tmp/round.json
go -C tools run ./reviewcard -pr 292 -round /tmp/round.json -details /tmp/findings.md
```

`go -C tools` runs from `tools/`, so give absolute paths. `-out DIR`
instead of `-pr` writes the cards and the comment body to `DIR` without
posting.

While a round runs, its comment must be the lead's latest comment on the
PR. gh uploads images only by editing the caller's latest comment, and
`reviewcard` refuses rather than edit a different one. Post other notes
after the round ends.

The comment is created when the round starts, so that is when watchers are
notified; the verdicts arrive as edits, which notify no one.

### The findings tables

Blocking findings go under Critical Issues; everything else goes under
Non-blocking Suggestions. A Critical Issues row has no Status column, so
its description ends with the status (open, fixed in a named commit, or
withdrawn with the reason). A suggestion's Status is fixed in a named
commit, `Filed as #N`, or open until that issue is filed. A table with no
rows is replaced by "None.".

When a later push settles an open item, the lead edits that round's tables
(`gh api -X PATCH repos/{owner}/{repo}/issues/comments/ID -F body=@FILE`,
leaving the `<picture>` as it is); the cards keep the verdict as reviewed.
The edit notifies no one, so the next round's details start with a line
naming what it settled, for example "Settles from Round 1: ...". Someone
reading only the PR sees what the reviewers caught and how each item was
settled.

The `-details` file:

````markdown
<!-- Use `<details open>` when critical issue count > 0, else `<details>`. -->
<details [open]>

<summary>Critical Issues: [count]</summary>

<!-- Sort from most to least severe. -->
| # | File | Line | Reviewer | Issue | Severity |
|---|------|------|----------|-------|----------|
| 1 | [file] | [line] | [agent] | [description and status] | 🔴 Critical |

</details>

<!-- Use `<details open>` when critical issue count is 0 and suggestions count > 0, else `<details>`. -->
<details [open]>

<summary>Non-blocking Suggestions: [count]</summary>

| # | File | Line | Reviewer | Suggestion | Category | Status |
|---|------|------|----------|------------|----------|--------|
| 1 | [file] | [line] | [agent] | [description] | Performance | Filed as #130 |

</details>
````

The blank lines after `<details>` and after `</summary>` are required for
GitHub to render the Markdown inside. Leave the HTML comments out of the
posted comment.

## Merge

The lead merges once CI passes (or the change needs no tests), the assigned
reviewers approve, and every round's tables show each finding as fixed,
withdrawn or linked to an issue.

## Milestones

Milestones close only with Doug's review and approval.

## Shared files

Shared files are owned by the lead: `proto/`, `gen/go`, `pkg/contracts`,
`docs/spec/contracts.md`, `go.mod`/`go.sum`, `Makefile`, `AGENTS.md`,
`docs/review-process.md`, `docs/telemetry.md`. Other contributors propose
changes to them through the lead. Contract changes (`pkg/contracts`,
`proto/`, `docs/spec/contracts.md`) land first, in their own PR, before
code that depends on them.
