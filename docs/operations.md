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
  executable that only its owner can change, checked once at start; the server
  never looks one up on a `PATH` and never discovers one. `declaration` is the
  adapter's `AdapterDeclaration` in ProtoJSON (the reference ones are in
  `testdata/declarations/`), and its `name` must be the adapter's name. Until
  the adapter service lands an adapter is a stdio child process, one for each
  source, started on its first use and started again, after a pause that grows
  from a second to a minute, if it fails.
- **Secrets** are references, never values: `env:NAME`, where `NAME` matches
  `secrets.env_allowlist` (default `BEARING_SECRET_*`). A reference to a
  variable that is not set, or is empty, stops the start and names the
  variable. A source's adapter is started with a fixed `PATH`, the variables
  its own `spec.secrets` name, and the variables its adapter entry lists in
  `env`, and nothing else. The adapter reads a secret from the variable of the
  same name, so the GitHub adapter's `token_env` setting above names the
  variable. A `webhook_secret` is read only by the server, which checks each
  delivery with it; the adapter never receives it.

## Starting and stopping

The server takes the store's instance lock before it runs anything and gives it
up when it stops; a second server on the same store is refused. It stops on
SIGINT or SIGTERM, lets requests in flight finish for a few seconds, asks each
adapter to exit and kills one that does not, then releases the lock.

At start it warns about settings that loosen a control: `insecure_api_plaintext`
(tokens cross the network unencrypted unless a proxy terminates TLS),
`insecure_no_peer_rate`, and a webhook source with no schedule. GitHub does not
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
  whole API: raise all three. A limit that is present must be positive;
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
