# Running `bearing server`

`bearing server --settings FILE` runs the always-on part of Bearing: the
workers that apply the event log, the scheduler, the API on a Unix socket and
a TCP listener, and the webhook route. This page is for the person who runs it.
The design is in [ADR 7](adr/0007-durable-event-log.md), the routes in
[spec/api.md](spec/api.md) and the controls in
[security/threat-model.md](security/threat-model.md).

## The settings file

The settings file holds what must be known before the store opens: where the
store is, who may call the API, which adapters are installed and which sources
read through them. It is JSON, and a field the server does not know is an
error, so a misspelled setting cannot quietly leave a default in force. The
server refuses to start if the file can be written by its group or by others,
or is owned by anyone but the service user or root, since it names the
programs the server runs.

```json
{
  "store": { "graph": "postgres://bearing@db.internal/bearing?sslmode=verify-full" },
  "listen": {
    "socket": "/run/bearing/api.sock",
    "addr": "0.0.0.0:8443",
    "tls_cert": "/etc/bearing/tls.crt",
    "tls_key": "/etc/bearing/tls.key"
  },
  "auth": {
    "issuer": "https://idp.example.com/application/o/bearing/",
    "audience": "bearing",
    "access_token_jwt": true,
    "grants": [
      { "group": "bearing-admins", "role": "admin" },
      { "client": "ci", "role": "ingest", "sources": ["github-acme"] }
    ]
  },
  "audit": {
    "checkpoints": {
      "file": "/var/lib/bearing/audit/checkpoints.ndjson",
      "signing_key_env": "BEARING_AUDIT_SIGNING_KEY",
      "signing_key_id": "audit-2026"
    },
    "retention": { "age": "P365D" }
  },
  "adapters": [
    {
      "name": "github",
      "command": "/usr/local/bin/bearing-adapter-github",
      "declaration": "/etc/bearing/declarations/github.json",
      "env": ["HTTPS_PROXY"]
    }
  ],
  "sources": [
    {
      "apiVersion": "bearing/v1alpha1",
      "kind": "Source",
      "metadata": { "name": "github-acme" },
      "spec": {
        "adapter": "github",
        "schedule": "PT1H",
        "settings": { "org": "acme", "token_env": "BEARING_SECRET_GITHUB_TOKEN" },
        "secrets": { "token": "env:BEARING_SECRET_GITHUB_TOKEN" },
        "webhook_secret": "env:BEARING_SECRET_GITHUB_WEBHOOK"
      }
    }
  ]
}
```

- **Store.** `store.graph` is required. The password comes from
  `BEARING_STORE_PASSWORD`, never from the URL. A durable graph with an
  in-memory `store.events` is refused: a restart would lose the events the
  durable log exists to keep.
- **Sources** are `Source` resources (`apiVersion: bearing/v1alpha1`,
  `kind: Source`), the same objects the configuration resources of a later
  release read from the store, so moving to them is a copy, not a rewrite.
  `spec.schedule` is an ISO 8601 duration (`PT1H`, `P1D`, at least a minute);
  leave it out for webhooks and manual syncs only. `spec.authority` and
  `spec.capabilities` are refused until then.
- **Adapters** are programs you installed. `command` is an absolute path to an
  executable owned by the service user or root and not writable by its group
  or others. That is checked once at start, so replace an adapter by stopping
  the server first; the server never looks one up on a `PATH` and never
  discovers one. `declaration` is the
  adapter's `AdapterDeclaration` in ProtoJSON, a file held to the settings
  file's rules (owned by the service user or root, not writable by others, at
  most 1 MiB; the reference ones are in `testdata/declarations/`), and its `name` must be the adapter's name. Until
  the adapter service lands an adapter is a stdio child process, one for each
  source, started on its first use and started again, after a pause that grows
  from a second to a minute, if it fails. It starts in `/`, not in the
  directory the server was started in, so name absolute paths in `args`.
- **Secrets** are references, never values: `env:NAME`, where `NAME` matches
  `secrets.env_allowlist` (default `BEARING_SECRET_*`; an entry is a literal name prefix followed by one `*`, and a prefix that could match one of Bearing's own variables is refused). A reference to a
  variable that is not set, or is empty, stops the start and names the
  variable. A source's adapter is started with a fixed `PATH`, the variables
  its own `spec.secrets` name, and the variables its adapter entry lists in
  `env`, and nothing else. The adapter reads a secret from the variable of the
  same name, so the GitHub adapter's `token_env` setting above names the
  variable. A `webhook_secret` is read only by the server, which checks each
  delivery with it; the adapter never receives it.

## The audit log

Every change to the graph leaves a record in a hash chain. A chain alone does
not stop someone who can write the database from rewriting all of it, so the
server also writes a signed **checkpoint** of the chain's head, every 15
minutes by default, to a file outside the database. A rewritten chain no
longer matches the checkpoints, and `bearing audit verify` says where.

- **A durable store needs `audit.checkpoints`.** The server refuses to start
  without it, unless `audit.insecure_no_checkpoints` is true (a warning at
  start says what that gives up). An in-memory store needs none.
- **The file** (`checkpoints.file`) is an absolute path. Its directory must
  exist, be owned by the service user or root and not be writable by its group
  or others, and so must the file. Keep it where the database's writers cannot
  reach: another host, an append-only mount, or a copy shipped off the host as
  often as you checkpoint. Whoever can edit this file can forge the evidence.
  `bearing audit checkpoint --out` and `bearing audit verify` can run beside
  the server: each locks the file for the moment it reads or appends. A crash
  can leave a last line without its newline; `verify` then prints one line
  saying the cut-off line was ignored, and the next append removes it (a whole
  checkpoint that only lacks the newline is kept). A damaged line anywhere else
  still fails and names the line.
  A process that holds the file's lock for over 30 seconds (a stopped command, a
  hung network mount) makes a write or a read fail and name the file. Archive the
  file only with the server stopped or after it has restarted on the new one.
- **The key** is an Ed25519 key as a PKCS #8 PEM in the variable
  `signing_key_env` names (`openssl genpkey -algorithm ed25519`), with an ID
  for it in `signing_key_id`. The variable must start with `BEARING_AUDIT_`,
  which the secret allowlist (`BEARING_SECRET_*`) can never match, so no
  source and no adapter can be given it. The server reads it again at every
  write, so a key that stops loading writes nothing and counts a failure
  (`bearing.audit.checkpoints`), never an unsigned line. To rotate, restart
  with the new key under a **new** `signing_key_id` and list the old public key
  under `retention.public_keys`; reusing an ID makes the older checkpoints fail
  verification. Keep the public half for `bearing audit verify --key ID=FILE`.
  The key sits in the server's environment, so an adapter running as the same
  user could read it from `/proc` (C-OPS-5). Unsigned checkpoints need
  `audit.insecure_unsigned_checkpoints` and cannot be combined with retention.
- **Retention** (`audit.retention`) is off unless the block is present. It cuts
  records older than `age` (default a year, at least a day) in batches, and
  before every cut it appends a signed marker to the same file and verifies the
  log against the file's checkpoints. The signing key is the writer's. List
  the public keys of earlier keys still in the file under `public_keys`
  (`{"audit-2025": "/etc/bearing/audit-2025.pem"}`); after a rotation keep
  them until their checkpoints are archived.

If the writer's metric reports `diverged`, the log's head no longer extends the
newest checkpoint in the file: a restore, a truncated tail, a rewritten chain,
or a stray line with a high number in the file. Do not clear it before you know
which.

1. Run `bearing audit verify --checkpoints FILE --key ID=FILE ...` and read where
   it fails.
2. If the log is the genuine one (a restore you meant to do), archive the file
   (keep it; it is the evidence) and start a new one at the same path.
3. If retention had cut the log, run `bearing audit reanchor --expect-seq N
   --key-id ID --key-env VAR --out FILE` for the new file (N is the number of
   the last record that was cut, from the archived file or a backup; the
   command needs the signing key in `VAR`, a `BEARING_AUDIT_` variable), so
   `verify` accepts the log's start.
4. Restart the server: the writer keeps the newest pinned head in memory and
   reads the file only at its first run.

## Starting and stopping

The server takes the store's instance lock before it runs anything and gives it
up when it stops; a second server on the same store is refused. It stops on
SIGINT or SIGTERM, lets requests in flight finish for a few seconds, asks each
adapter to exit and kills one that does not, then releases the lock.

At start it warns about settings that loosen a control: `insecure_api_plaintext`
(tokens cross the network unencrypted unless a proxy terminates TLS; when the
address is not a loopback one and any of the three limits below is still at its
default, it also says the defaults cap the whole API behind a proxy),
`insecure_no_peer_rate`, `audit.insecure_no_checkpoints`, unsigned checkpoints,
and a webhook source with no schedule. GitHub does not
resend a delivery the server refused, so a source that takes webhooks should
also have a `schedule` that catches up on what was missed.

## Sizing

- **Open files.** Each connection and each adapter pipe holds a file. Allow at
  least about 2,048 open files (`LimitNOFILE=4096` in a systemd unit).
- **Connections and requests.** The TCP listener holds at most `max_conns`
  connections (default 1,024), at most `max_conns_per_peer` from one address
  (default 64; an IPv6 peer counts as its /64), and answers each peer at most
  `peer_rate_per_minute` requests (default 1,200) before it looks at a token.
  Behind a proxy or ingress every client is one peer, so these defaults cap the
  whole API: raise all three. The server cannot tell a proxy from a client, so
  its start-up warning about this is a hint, not a check: it appears for plain
  HTTP on a non-loopback address while any one limit is at its default, and
  never for a listener that serves TLS itself, even behind a proxy that
  re-encrypts. A limit that is present must be positive;
  turning the per-peer rate off takes `insecure_no_peer_rate: true`.
- **Webhooks.** `webhooks.max_in_flight` (default 256) deliveries are held at
  once, each up to `webhooks.max_body_bytes` (default 1 MiB), so the worst case
  is their product in memory, 256 MiB by default, before any is checked.
  Lower either on a small host.
- **The socket directory.** The directory of `listen.socket` must be mode 0700
  and owned by the user the server runs as, and only that user, and root, can
  open the socket. The server refuses to start otherwise. The adapter child
  runs as the same user and can call the socket, so install only adapters you
  trust until adapters get their own user.
