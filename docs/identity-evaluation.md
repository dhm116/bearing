# Identity evaluation: results

How well does Bearing decide that two records (a GitHub team and a directory
group, say) describe one thing? `internal/identityeval` builds a set of record
pairs whose answer is known, runs `match.Run` with the default settings
(the `Transparent` matcher, propose at 0.6, floor 0.1, half-life 30 days) and
scores what it proposes. The labels are complete: a pair no case names is
different, so proposing it is wrong.

Re-run with `go test ./internal/identityeval -run Eval -v`. The tests fail if
precision, recall or candidate recall drop under a floor set a little below
the numbers here, so a regression shows. Numbers are for seed 1.

## The words

- **Precision**: of the proposals open, how many are right.
- **Recall**: of the pairs that really are one thing, how many are proposed.
- **Candidate recall**: of the same pairs, how many the cheap first pass
  (`match.Candidates`: similar names or a shared member) even offers to the
  matcher. A pair missed here can never be proposed.
- A dash means the group has no true pairs, so there is nothing to recall.

## Results by group

| Group | Cases | True | Precision | Recall | Candidate recall |
| --- | --- | --- | --- | --- | --- |
| acme | 12 | 11 | 0.917 | 1.000 | 1.000 |
| convention (same team, another naming style) | 50 | 50 | 1.000 | 0.580 | 0.960 |
| rename (same team, new name) | 10 | 10 | 1.000 | 0.400 | 0.600 |
| near-miss (similar names, different teams) | 12 | 0 | 0.000 | - | - |
| unrelated | 20 | 0 | 1.000 | - | - |
| person-same-email | 15 | 15 | 1.000 | 1.000 | 1.000 |
| person-alias (login or nickname, same email) | 10 | 10 | 1.000 | 0.000 | 0.000 |
| person-same-name (different people) | 15 | 0 | 0.000 | - | - |
| person-different | 15 | 0 | 1.000 | - | - |
| **all** | 159 | 96 | 0.747 | 0.615 | 0.833 |

A group's precision counts only the proposals for its own pairs. A group with
no true pairs and precision 0.000 means every proposal there is wrong.

## Where the matcher is weak

- **Near-miss names** (`ledger`/`legend`): each one is proposed. Name
  similarity alone cannot tell them from a rename.
- **Different people with one name** (`person-same-name`): every one is
  proposed. The matcher has no feature that says "same name, different
  email, so probably two people". Proposals wait for review, so this costs
  reviewers' time and does not corrupt the graph, but it is the weakest spot.
- **Renames**: recall 0.40, and candidate recall 0.60. A renamed team shares
  no name with its old self, so it is only found when members overlap
  (the zero-overlap renames are never offered).
- **Naming conventions** (`Payments Team`, `payments-team`): recall 0.58
  although candidate recall is 0.96, so here the scoring is the weak step, not
  the first pass: the extra words pull the name score under 0.6 unless members
  or an email help.
- **Person aliases** (`jane` for `Jane Doe` with the same email): never
  offered as candidates, because candidates look at names and members, not
  at emails. A shared email is a feature for scoring only.

None of this was tuned away. The key rules (`match.KeyRule`) cover the cases
a key pattern can name exactly, such as the Acme teams.

## Decay against none

Five scenarios play sixteen weekly rounds, with the change in round 4 (day
28): the matcher runs after each round's ingest, once with the default
half-life (30 days) and once with a half-life of a hundred years ("none").
Stale lag is how long a proposal outlived the end of its pair, to the first
round without it; "stuck" counts proposals still open at the end; flaps are
proposals that closed and reopened.

| Scenario | Decay | Pairs ended | Mean lag | Max lag | Stuck | Flaps | Worst precision |
| --- | --- | --- | --- | --- | --- | --- | --- |
| stable | default / none | 0 | - | - | 0 / 0 | 0 / 0 | 1.000 / 1.000 |
| renamed | default / none | 0 | - | - | 0 / 0 | 0 / 0 | 1.000 / 1.000 |
| membership-ended | default / none | 0 | - | - | 0 / 0 | 0 / 0 | 1.000 / 1.000 |
| deleted-github | default / none | 3 | 63 days / - | 63 days / - | 0 / 3 | 0 / 0 | 0.000 / 0.000 |
| deleted-directory | default / none | 3 | 63 days / - | 63 days / - | 0 / 3 | 0 / 0 | 0.000 / 0.000 |

With decay, a proposal whose record was deleted closes about two
half-lives after the deletion; without it, it never does. (Worst precision
is 0 in the deleted scenarios because the stale proposals are the only
open ones until they close.) The test fails if any of them is still open
at the end with the default half-life.

What makes this work is how a deposit is dated (`pkg/match`, ADR 7: only
new observations deposit):

- Its time is the newest observation behind the features it scored: the
  records' own observations for the name, the `member_of` facts for
  members, the email facts for email.
- It counts only while both records are current: the pair takes the
  earlier of the two sides, and a side whose last observation deleted it
  has none, so a deleted record stops reinforcing at once.
- A pair gets a new deposit only when that time is newer than its last
  deposit, and then only if the feature values changed or a half-life has
  passed. A matcher re-running on the same evidence adds nothing.
- Ingest's own links (`linked_ids`, `shared_key`) get at most one deposit
  per source and half-life, not one per sync.

Before this, evidence was dated by the newest fact touching either
record's cluster, so the surviving record kept every stale proposal alive.

### Hourly syncs

`Hourly` syncs one team in GitHub and the directory every hour for 60
days; GitHub deletes the team on day 20. The matcher runs daily, with a
half-life of 7 days for teams so the 40 days after the deletion hold
several.

| Scenario | Half-life | Pairs ended | Lag | Stuck | Flaps |
| --- | --- | --- | --- | --- | --- |
| hourly | 7 days | 1 | 18 days (2.6 half-lives) | 0 | 0 |

The test fails if the proposal outlives the deletion by more than four
half-lives. Without the once-per-half-life rule, the daily deposits pile up
and the lag is 40 days (5.7 half-lives).

## Teams across sources

`internal/e2e` `TestAcmeTeamsAreOneAcrossSources` syncs GitHub, the directory
and Kubernetes into one store and runs `match.Run` with two key rules
(`authentik:group_name/{name}` with `github:team/acme/{name}`, and
`kubernetes:team/{name}` with the same GitHub pattern). The result: one entity
per team carrying all its keys, Jane Doe a member of the payments team once,
and the team CODEOWNERS names for `payments-api` is the team its Kubernetes
workload's `team` label names.
