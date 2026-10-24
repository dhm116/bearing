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

## linked_ids_complete is ignored

- **Found in:** step 2 ingest; still true after ADR 7 (linked IDs now open `same_as` links, not merges)
- **Area:** identity
- **Example:** a GitHub user unlinks an email; the next observation sends the remaining `linked_ids` with `linked_ids_complete`, but the link to the old email stays open.
- **Impact:** an asserted link the source withdrew keeps two records joined until someone closes it. A proposed one decays.
- **Proposed fix:** when `linked_ids_complete` is set, close this source's `linked_ids` links to keys no longer listed.

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

## Slow adapter starts hold the source's lock

- **Found in:** step 3 host
- **Area:** adapters
- **Example:** an adapter takes 8 s to answer `Describe` after a restart.
- **Impact:** calls to that source wait up to 10 s, ignoring their own context.
- **Proposed fix:** start the process outside the lock and let callers wait on their context.

## Long socket paths

- **Found in:** step 3 host
- **Area:** adapters
- **Example:** a long source name under macOS's long `TMPDIR` exceeds the 103-byte socket path limit.
- **Impact:** `Start` refuses with a clear error.
- **Proposed fix:** hash long names, or use a shorter base directory.

## Remote adapters retry without backoff

- **Found in:** step 3 host
- **Area:** adapters
- **Example:** a remote adapter is down for an hour.
- **Impact:** each scheduled call dials again at once; no backoff between failures.
- **Proposed fix:** the restart backoff local adapters use, applied to dials.

## Manifest parsing limits

- **Found in:** step 3 GitHub (`adapters/github/manifests.go`)
- **Area:** language versions
- **Example:** `go 1.22rc1`; `project.requires-python` as a dotted key outside `[project]`; manifests in a monorepo's subdirectories; a push that only changes a manifest.
- **Impact:** pre-releases order wrongly, the dotted key and subdirectories are missed, and `runtime` refreshes only at the next sync.
- **Proposed fix:** `golang.org/x/mod/semver`, a TOML parser, reading known subdirectories, and refreshing on pushes that touch manifests.

## Kubernetes adapter limits

- **Found in:** step 3 Kubernetes (`adapters/kubernetes`)
- **Area:** Kubernetes
- **Example:** team labels in mixed case; a repository annotation like `TODO`; several `ghcr.io` images; objects created or deleted mid-sync; other registries.
- **Impact:** label values are used as written; a bad annotation silently falls back to the image; the first image wins; a sync can see a half-changed list; only `ghcr.io` images hint at a repository.
- **Proposed fix:** normalize label values; report bad annotations; consistent reads with `resourceVersion`; a configurable registry-to-repository mapping.

## Kubernetes dependencies the adapter can't see

- **Found in:** step 3 dependencies (`adapters/kubernetes`)
- **Area:** dependencies
- **Example:** `DATABASE_URL` in a ConfigMap (`envFrom`) or a Secret (`valueFrom`); an `ExternalName` Service pointing at RDS; `postgres-0.postgres.billing` (a StatefulSet pod's DNS name); a non-default cluster domain; references into an excluded namespace; init containers and command-line arguments.
- **Impact:** those dependencies are missing; external databases and managed services never appear.
- **Proposed fix:** read ConfigMap values the same way (never Secrets); emit `ExternalName` targets as `CloudResource`s; match pod DNS names and a configured cluster domain.

## Broad Service selectors

- **Found in:** step 3 dependencies
- **Area:** dependencies
- **Example:** a Service selecting `app: billing` also matches a second workload carrying that label.
- **Impact:** a false `depends_on` to every workload the selector matches.
- **Proposed fix:** prefer the workload whose labels match the selector most exactly, or propose rather than assert when several match.

## The Kubernetes sync lists workloads twice

- **Found in:** step 3 dependencies
- **Area:** Kubernetes
- **Example:** a cluster with thousands of workloads and Services.
- **Impact:** an index pass and an emit pass double workload list requests, and the index travels in the sync cursor, which grows with the cluster.
- **Proposed fix:** keep the index in the adapter process between pages, or read Services and workloads into one pass per namespace.

## Qualifiers after a merge

- **Found in:** step 3 qualifiers (`internal/duckstore`)
- **Area:** facts
- **Example:** two merged entities both held `depends_on ledger`, with different ports.
- **Impact:** the surviving fact keeps the oldest copy's qualifiers, not the newest. Numbers read back as floats.
- **Proposed fix:** take the newest source's qualifiers when collapsing, as the query layer does when folding.

## Reverse alias fold

- **Found in:** identity review (`internal/duckstore/ingest.go`, `resolve`)
- **Area:** identity
- **Example:** record A (a Team) lists `x:team/1` as an alias. Later a source observes `x:team/1` as a primary key.
- **Impact:** the new observation lands on A, because a key already bound to a record is never moved. It is order-dependent and cannot be undone. No adapter emits this today.
- **Proposed fix:** make a new record for the primary key and open an asserted `shared_key` link to A. This needs a decision on whether the key binding moves.

## Observation IDs collide across sources

- **Found in:** identity review (`pkg/model/model.go`)
- **Area:** ingest
- **Example:** two sources observe `x:repo/1` at the same instant; the ID is `key@time`.
- **Impact:** the second observation is dropped as a duplicate, with its facts and sources. Rare: needs the same key and the same timestamp.
- **Proposed fix:** put the source in the ID (`source/key@time`).

## Aliases never move

- **Found in:** identity review
- **Area:** identity
- **Example:** team `@acme/old` is renamed `@acme/new`, and a new team takes the name `@acme/old`. CODEOWNERS lines with `@acme/old` still resolve to the renamed team.
- **Impact:** owner answers name the wrong team until the alias is dropped from the first record.
- **Proposed fix:** when a new primary key claims a name that is another record's alias, propose a `distinct_from` link and let the claim win once the old record stops listing it.

## Ingest cost per observation

- **Found in:** identity review
- **Area:** store
- **Example:** an hourly sync of a large org: tens of thousands of observations.
- **Impact:** about 8 ms per observation (about 0.4 ms per DuckDB statement), so 50,000 observations take about 7 minutes.
- **Proposed fix:** batch statements (one insert per table per page, not per observation) before big orgs sync hourly.

## The API's TLS certificate is read once

- **Found in:** security review (`pkg/server/apilisten.go`)
- **Area:** TLS
- **Example:** the API certificate is renewed on disk.
- **Impact:** the server keeps the old one until restarted, so an expired certificate needs a restart. (Remote adapter certificates are reloaded, `pkg/adapter/tls.go`.)
- **Proposed fix:** a `GetCertificate` callback that re-reads the files when they change.

## Kubernetes Secret mounts are group-readable by default

- **Found in:** security review (`pkg/server/boot/assemble.go`, `checkTLSFiles`)
- **Area:** deployment
- **Example:** a Secret volume's `defaultMode` is 0644, so a mounted adapter key is readable by others.
- **Impact:** `Dial` refuses the key until the volume sets `defaultMode: 0400`. The failure is loud, but the operator has to know why.
- **Proposed fix:** say it in the operations doc, with a manifest snippet.

## Refused TLS handshakes are logged at Debug only

- **Found in:** security review (`pkg/server/apilisten.go`, `newHTTPServer`)
- **Area:** API
- **Example:** a scanner opens connections and fails the TLS handshake over and over.
- **Impact:** the handshake error goes to the Debug log, not to Info or a metric, so nobody sees a scan. The log is not flooded either, and the per-peer connection caps already bound the cost, so there is no rate-limit worry now.
- **Proposed fix:** if scans matter, count refused handshakes in a metric (never labelled with the peer).

## Remote adapter trust and protocol limits

- **Found in:** security review
- **Area:** adapters
- **Example:** (a) a remote adapter in a Kubernetes cluster is trusted through its own CA file only; the cluster CA and service accounts are not used; (b) the adapter protocol version is not part of the gRPC `Describe` answer (the Go constant `adapter.ProtocolVersion` is not sent); (c) there is a short window between creating a local adapter's socket and setting its mode (the directory is already 0700); (d) a webhook is replayed after the log's retention has dropped its first copy; (e) the API is HTTP/1.1 only, and this is not tested against every client.
- **Impact:** (a) operators manage CAs by hand; (b) a core and an adapter that disagree find out at the first bad call; (c) none while the directory stays private; (d) the replay is applied again; (e) unknown.
- **Proposed fix:** (a) accept the cluster CA; (b) send a version in `Describe` and refuse a mismatch; (c) set the umask around `Listen`; (d) keep delivery IDs longer than events; (e) test the clients that matter.

## Related makes one query per step

- **Found in:** security review (`pkg/query/query.go`)
- **Area:** queries
- **Example:** `related` at depth 5 on a service with many dependencies.
- **Impact:** one `Entity` query for each distinct entity the walk reaches (N+1; repeats are cached). It is bounded by the slow-read cap and the 15 s deadline, but it is slow on a wide walk.
- **Proposed fix:** load all the entities a walk names in one query.

## Pages can go stale during a long sync

- **Found in:** sync review (`pkg/server/events.go`)
- **Area:** sync
- **Example:** an adapter pages a large org over several minutes; a repository changes between page 1 and page 9.
- **Impact:** the sync mixes times, and a late page can show an older state than an earlier page. The next sync fixes it.
- **Proposed fix:** have the adapter report one `sync_started` time and skip items changed after it, or read from a consistent snapshot where the source has one.

## Deposits mixing rules on one link

- **Found in:** matcher review (`internal/duckstore/links.go`, `upsertLink`)
- **Area:** identity
- **Example:** a key rule and the name matcher both support the same pair of records.
- **Impact:** a pair has one open `same_as` link, so both rules' deposits go on it and are scored together. The link's `rule` is the one that opened it (or the one that promoted it to asserted), so the rule name does not say who did most of the work.
- **Proposed fix:** keep the rule on each deposit, and show it in the link's evidence.

## Concurrent DuckDB queries crash the process

- **Found in:** step 4 (a `pkg/server` test under load)
- **Area:** store
- **Example:** an event-log read (`Partitions`) while a worker commits an offset in a write transaction on the same store; SIGSEGV inside `duckdb_execute_pending` (duckdb-go v2.10506.0), about 1 in 20 race-detector runs of the server tests, on the commit before step 4 as well.
- **Impact:** the whole server process dies. Worked around by limiting the store to one connection (`internal/duckstore/duckstore.go`): no crash in 20 runs, but every query now waits for the one before it, so a long sync write delays API reads.
- **Proposed fix:** reproduce outside Bearing (two goroutines, one database, a read during a write transaction, with and without context cancellation) to tell DuckDB from the driver's interrupt handling; report upstream; then restore a pool, or keep one writer connection and a pool of readers.
- **Tried:** the duckdb-go 2.0 preview (v2.20000.0-6.preview, the newest, from 2026-08-28) with the pool restored crashed in 2 of 40 race-detector runs of the server tests; the current driver with the pool crashed in 0 of 40 on the same quiet machine, 1 of 20 earlier, and 12 of 20 under heavy load. The crash depends on load and timing; the preview doesn't fix it.
- **See also:** [ADR 8](adr/0008-ducklake-not-now.md) keeps the single connection. Its DuckLake spike saw no crash in 13 runs, which hints the bug is in DuckDB's storage layer, but proves nothing.

## A rule or ingest can push a record out of its cluster

- **Found in:** link review, round 2 (`internal/duckstore/links.go`, `recluster`)
- **Area:** identity
- **Example:** a person joins A and B, then splits B from C. A key rule later asserts A ~ C with evidence older than the join. Clustering takes links oldest first, so A ~ C wins, and A ~ B is then blocked by B's `distinct_from` against C: B leaves A's cluster.
- **Impact:** B's answers change and nothing records why; the person's join is still open but no longer holds. A person's join, accept or undo of a `distinct_from` is refused when it would do this, but `Link`, `CloseLink` and `Ingest` are not: closing a link can change which of the remaining links wins, as undoing a `distinct_from` can.
- **Proposed fix:** have `Link`, `CloseLink` and `Ingest` compare cluster membership before and after, as `Decide` does, and put a link that would push a record out on the review queue instead of asserting it; or order clustering by when a link became asserted rather than when it opened.

## Processes running as the service user can decide links

- **Found in:** link review, round 2 (`pkg/server/apilisten.go`, C-API-8)
- **Area:** security
- **Example:** a local adapter (same UID as the server) double-forks a process that re-parents to init and connects to the API socket.
- **Impact:** the socket admits it as an administrator and, with `api.link_decisions: socket`, it can join or split records. The server refuses its own descendants by PID (Linux), but a detached one is no longer a descendant.
- **Proposed fix:** run local adapters under their own UID (`SysProcAttr.Credential`), so the socket's UID check refuses them outright. A cheaper partial fix is to make the server a child subreaper (`PR_SET_CHILD_SUBREAPER`), so a detached process is re-parented to the server and stays its descendant.
