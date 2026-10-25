# Review and merge process

How a Bearing pull request is reviewed, how the review is shown on the PR,
and when it merges. [`AGENTS.md`](../AGENTS.md) points here; read both
before reviewing or merging.

## Reviewers

Each PR gets up to three expert reviewers, chosen from
[`.github/reviewers.yml`](../.github/reviewers.yml) by the paths it touches,
plus the ponytail reviewer, which doesn't count toward the three. Size the
panel to the change: one expert for a small or routine PR, up to three for
the data model, the store, adapters' trust boundaries or security. If
`security` matches, it always takes a slot. When more experts match than
the PR warrants, the lead picks the most relevant and says why in the PR
description. `go` reviews any Go change that no domain expert covers. A PR
that matches no rule gets the file's `default`. Reviewer definitions live
in [`.claude/agents/`](../.claude/agents/).

The experts run **in parallel**, and they only read: the diff, the code it
touches and the docs. They never run tests, builds or linters; CI does
that.

## Findings

Every finding states severity, effort to fix now, how hard it is to change
later, and whether leaving it opens an important gap against the project's
goals. A defect that makes the change wrong, unsafe, or not do what it
claims (a bug, a missing check, a test that cannot fail) leaves an
important gap. Style and polish do not. Only findings that are hard to
change later or leave an important gap block; the rest become follow-up
issues. The lead files them as GitHub issues linked from the PR before
merging.

**Proportion.** Fix the 80–90% case now. A finding about an edge case that
is unlikely in practice, and costs much more to handle than it risks, does
not block: it goes into [`edge-cases.md`](edge-cases.md) with its example
and proposed fix, and the table's Status names that entry.

## Rounds

A round is one push answering the findings plus the reviewers' re-review.
Up to five rounds per PR. The first review is Round 1, so push k's
re-review is Round k+1 and the fifth push ends with Round 6. After the
fifth push's round ends with blocking findings open, the lead stops and
mentions @dhm116 (Doug, the maintainer) in a new comment: the round's own
comment is updated by editing, and an edit notifies no one.

The ponytail reviewer (the `ponytail-review` skill from the ponytail
plugin) is the round's referee. As each expert's review arrives, ponytail
weighs it: is each finding real, is it worth fixing now, does the fix cost
more than the risk, and does it belong in the edge-case log instead. When
the last expert is in, ponytail gives its own verdict and **one summary**
of the round: which findings block, which become issues, which go to the
edge-case log. The lead starts no fix and no next round until that summary
exists. Ponytail has a card of its own; `reviewcard` refuses a round that
doesn't list it.

## Review comment

Every round, including the first review and one that approves outright,
has one PR comment, posted and kept current with
[`tools/reviewcard`](../tools/reviewcard/main.go):

1. **The round starts.** Before the reviewers run, the lead posts the round
   with every reviewer `running` and `ponytail` `queued`. The first post
   for a round hides the lead's earlier review comments on the PR
   (minimized as outdated, never deleted), so the PR shows only the
   current round.
2. **An expert finishes.** The lead runs it again with that reviewer's
   verdict (`approve`, `discuss` for Needs Discussion, or `changes` for
   Request Changes), each blocking finding in a few words, and its number of
   suggestions, and marks `ponytail` `running` while it weighs that review.
   This edits the same comment. Blockers shown are the ones ponytail
   agrees block.
3. **The experts are done.** Ponytail writes its verdict and the round's
   summary.
4. **The round ends.** The lead posts ponytail's verdict and the findings
   tables (`-details`, starting with ponytail's summary) in one run. Once every reviewer is done,
   `reviewcard` refuses a run without `-details`, so a later correction
   keeps the tables too.

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
  {"name": "go", "state": "running"},
  {"name": "ponytail", "state": "queued"}]}
EOF
go -C tools run ./reviewcard -pr 292 -round /tmp/round.json
go -C tools run ./reviewcard -pr 292 -round /tmp/round.json -details /tmp/findings.md
```

`go -C tools` runs from `tools/`, so give absolute paths. `-out DIR`
instead of `-pr` writes the cards and the comment body to `DIR` without
posting.

While a round runs, its comment must be the lead's latest comment on the
PR. gh can add images to an existing comment only by editing the caller's
latest one, and `reviewcard` refuses rather than edit a different one. Run
updates one at a time, and post other notes after the round ends. A note
the lead owes before the round, such as why it picked two of several
matching reviewers, goes in the PR description.

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

Shared files are owned by the lead: `proto/`, `gen/go`, `go.mod`/`go.sum`,
`Makefile`, `AGENTS.md`, `ROADMAP.md`, `docs/adr/`,
`docs/review-process.md`, `docs/telemetry.md`, `pkg/store` and `pkg/graph`.
Other contributors propose changes to them through the lead. Changes to
`proto/` and the store interface land first, in their own PR, before code
that depends on them.
