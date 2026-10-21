# Edge cases

Cases found during work or review that were deliberately left for later,
so the common case could ship. Add one when a fix would cost more than it's
worth now. Remove it when it's fixed, naming the PR in the commit.

Each entry:

```markdown
## Short name

- **Found in:** PR or issue link
- **Area:** package or feature
- **Example:** a concrete input or sequence that triggers it
- **Impact:** what goes wrong, and how often it's likely to happen
- **Proposed fix:** the approach, and anything already known about it
```

## Keys of different kinds in one observation

- **Found in:** step 2 ingest (`internal/duckstore/ingest.go`)
- **Area:** identity
- **Example:** an observation's primary key `a:x/2` is bound to a Team and its alias `a:x/1` to a Component.
- **Impact:** no merge; the primary key's entity is used and the other key stays where it is, silently. Rare: needs a source to reuse an ID across kinds.
- **Proposed fix:** count conflicts in `IngestResult`, or record a proposed `distinct_from` fact for review.

## Stale entity names

- **Found in:** step 2 ingest
- **Area:** identity
- **Example:** an observation older than the last one renames the entity back.
- **Impact:** a wrong display name until the next observation. Answers are unaffected.
- **Proposed fix:** keep the name's observed time and skip older names.

## Relation qualifiers are dropped

- **Found in:** step 2 ingest
- **Area:** facts
- **Example:** a `depends_on` relation carries `attributes` such as the port or protocol.
- **Impact:** "how does X depend on Y" can't show them.
- **Proposed fix:** a JSON `qualifiers` column on `fact`, written from `Relation.attributes`.

## linked_ids treated as keys

- **Found in:** step 2 ingest
- **Area:** identity
- **Example:** a GitHub user links an email the IdP also has; both entities merge at once, and `linked_ids_complete` is ignored.
- **Impact:** a wrong link merges two people with no way to undo it (there is no un-merge).
- **Proposed fix:** store linked IDs as proposed `same_as` facts and merge on a rule or a review; honour `linked_ids_complete`.

## Claims with valid_to before valid_from

- **Found in:** step 2 ingest
- **Area:** facts
- **Example:** an adapter sends a relation whose `valid_to` is earlier than its `valid_from`.
- **Impact:** an empty interval is stored as given.
- **Proposed fix:** reject it in `pkg/model` validation.

## Walk on dense graphs, and edges that close a cycle

- **Found in:** step 2 reads (`internal/duckstore/read.go`)
- **Area:** dependency walks
- **Example:** layered services where each depends on every service in the next layer; or `api -> ledger -> db -> ledger`, where `db -> ledger` is not returned.
- **Impact:** the recursive CTE enumerates paths, so time can grow quickly on dense graphs; a cycle's closing edge is hidden.
- **Proposed fix:** walk level by level with a visited set, and emit edges to visited entities without expanding them.

## Large IN lists in reads

- **Found in:** step 2 reads
- **Area:** reads
- **Example:** loading sources for tens of thousands of facts in one call.
- **Impact:** very long `IN (?, ?, …)` lists.
- **Proposed fix:** pass one list parameter and use `list_contains`, or a temporary table.

## Find scans every name and key

- **Found in:** step 2 reads
- **Area:** search
- **Example:** `Find` computes Jaro-Winkler similarity for every row.
- **Impact:** fine at MVP sizes; slow at hundreds of thousands of entities.
- **Proposed fix:** prefilter by prefix or trigram before scoring.

## Event log trim and read at scale

- **Found in:** step 2 event log (`internal/duckstore/eventlog.go`)
- **Area:** event log
- **Example:** thousands of partitions; or a read of 1,000 events of 8 MiB each.
- **Impact:** Trim runs one query per group per partition; Read may buffer more rows than the 32 MiB it returns.
- **Proposed fix:** one grouped query over `event_commit` for Trim; page the Read query.
