# Data model

Bearing models an engineering organization as **entities** connected by
typed, directed **relations**. Adapters report what they see as
**observations**; the core turns observations into **facts**.

## Entity kinds

| Kind | Meaning | Typical attributes | First sources |
| --- | --- | --- | --- |
| `Person` | A human | `name`, `login`, `email` | GitHub, PagerDuty |
| `Team` | A group of people | `name`, `slug`, `description` | GitHub teams, PagerDuty teams |
| `Repository` | A source repository | `url`, `default_branch`, `language`, `topics`, `archived` | GitHub |
| `Component` | Something built and run or used: a service, library, job or website | `type`, `tier`, `lifecycle` | GitHub, AWS tags, PagerDuty services |
| `Package` | A published package | `ecosystem`, `name`, `versions` | GitHub dependency graph |
| `Environment` | Where components run | `name`, `account`, `region` | AWS Organizations |
| `CloudResource` | An infrastructure resource | `arn`, `type`, `tags` | AWS |
| `Change` | A pull request, commit, deploy or config change | `kind`, `author`, `at`, `url` | GitHub, AWS CloudTrail |
| `Incident` | An operational incident | `severity`, `status`, `started_at` | PagerDuty |
| `Schedule` | An on-call rotation | `name`, `on_call_now` | PagerDuty |
| `Document` | A runbook, ADR, README or similar | `title`, `url`, `doc_type` | GitHub |

Attributes are open: adapters MAY add any attribute. The table lists
attributes that consumers can rely on when present.

## Relation types

Relations point from the observed entity to another entity.

| Type | From → To | Example |
| --- | --- | --- |
| `member_of` | Person → Team, Team → Team | jdoe is a member of payments |
| `owned_by` | anything → Team or Person | payments-api is owned by payments |
| `depends_on` | Component → Component, CloudResource | payments-api depends on ledger-svc |
| `publishes` | Repository, Component → Package | ledger-client repo publishes the ledger-client package |
| `consumes` | Component → Package | payments-api consumes ledger-client |
| `deployed_to` | Component, CloudResource → Environment | payments-api is deployed to prod-us-east |
| `runs_on` | Component → CloudResource | payments-api runs on an EKS cluster |
| `on_call_for` | Schedule, Person → Component, Team | payments-oncall is on call for payments-api |
| `affected_by` | Component → Incident | payments-api is affected by incident 4411 |
| `documents` | Document → anything | runbook.md documents payments-api |
| `changed_by` | Change → Person | PR 1182 was changed by jdoe |
| `defined_in` | Component, Document → Repository | payments-api is defined in acme/payments-api |

## Keys

Every entity in an observation is identified by a **key** scoped to its
source system:

```
<system>:<type>/<id>
github:user/jdoe
github:team/acme/payments
pagerduty:schedule/PX12AB
aws:resource/arn:aws:rds:us-east-1:123456789012:db:payments-prod
```

- `system` and `type` are lowercase letters, digits and hyphens (`type` also
  allows underscores). `id` is anything the source uses and MAY contain `/`.
- Keys are never shared across systems. An adapter MUST NOT guess another
  system's key. Deciding that `github:user/jdoe` and `pagerduty:user/PABC12`
  are the same Person is the core's job, not the adapter's.

## Observations

An observation is one adapter's report about one entity at one time. It is a
[CloudEvents 1.0](https://cloudevents.io) event, so it can travel over any
message bus. The JSON Schema is
[`schema/observation.v1.schema.json`](../../schema/observation.v1.schema.json).

```json
{
  "specversion": "1.0",
  "id": "github:repo/acme/payments-api@2026-09-28T01:30:00Z",
  "type": "dev.bearing.observation.v1",
  "source": "adapter/github",
  "time": "2026-09-28T01:30:00Z",
  "datacontenttype": "application/json",
  "data": {
    "entity": {
      "kind": "Repository",
      "key": "github:repo/acme/payments-api",
      "attributes": { "default_branch": "main", "language": "Go" }
    },
    "relations": [
      { "type": "owned_by", "to": "github:team/acme/payments",
        "attributes": { "pattern": "*", "file": ".github/CODEOWNERS" } }
    ],
    "evidence": { "url": "https://github.com/acme/payments-api" }
  }
}
```

Rules:

- An observation describes the entity as the source sees it **now**. It is a
  statement, not a diff: re-sending the same observation MUST be harmless.
- `relations` lists the relations the adapter can see from this entity. An
  adapter SHOULD report a relation from the side that owns the information
  (a repository's CODEOWNERS says who owns it, so the repository observation
  carries `owned_by`).
- To report that something is gone, set `entity.deleted: true`, or
  `relation.absent: true` for a single relation (for example a removed team
  membership). Absence of a relation from an observation is **not** a
  deletion, because an adapter may not see every relation every time.
- `evidence` SHOULD point at where a person can check the claim.
- `id` SHOULD be stable for the same entity and time so consumers can
  de-duplicate.

## Facts

The core resolves observation keys to entities and stores **facts**: a
subject, a relation, an object, a confidence between 0 and 1, and the
sources behind it. Facts at or above the assertion threshold (0.9 by
default) are used for answers and policy; lower ones are kept as hedges. The
history of every fact is retained. See
[`pkg/contracts`](../../pkg/contracts/contracts.go) for the Go types.
