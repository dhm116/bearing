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

Five scenarios play ten weekly rounds, with the change in round 4: the
matcher runs after each round's ingest, once with the default half-life and
once with a half-life of a hundred years ("none"). Stale lag is how long a
proposal outlived the end of its pair; "stuck" counts proposals still open at
the end; flaps are proposals that closed and reopened.

| Scenario | Decay | Pairs ended | Stuck | Flaps | Worst precision |
| --- | --- | --- | --- | --- | --- |
| stable | default / none | 0 | 0 | 0 | 1.000 / 1.000 |
| renamed | default / none | 0 | 0 | 0 | 1.000 / 1.000 |
| membership-ended | default / none | 0 | 0 | 0 | 1.000 / 1.000 |
| deleted-github | default / none | 3 | 3 / 3 | 0 | 0.000 / 0.000 |
| deleted-directory | default / none | 3 | 3 / 3 | 0 | 0.000 / 0.000 |

What this says: **decay changes nothing in these scenarios.** No flapping, no
harm (the test only fails if decay makes precision worse), but also no
benefit. When one side of a pair is deleted, the proposal never closes, with
or without decay. The likely cause is that evidence is dated by the newest
fact touching either record's cluster, so the surviving record (and its
members) keeps refreshing the proposal. A deleted record should stop
counting as evidence; that is the follow-up this result points to.
In the renamed scenario the proposals stay open too, which is correct: the
truth stays "same" while the names stop agreeing.

## Teams across sources

`internal/e2e` `TestAcmeTeamsAreOneAcrossSources` syncs GitHub, the directory
and Kubernetes into one store and runs `match.Run` with two key rules
(`authentik:group_name/{name}` with `github:team/acme/{name}`, and
`kubernetes:team/{name}` with the same GitHub pattern). The result: one entity
per team carrying all its keys, Jane Doe a member of the payments team once,
and the team CODEOWNERS names for `payments-api` is the team its Kubernetes
workload's `team` label names.
