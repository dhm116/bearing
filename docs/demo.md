# Run it: Bearing against the fake Acme org

`docker compose up` starts Bearing and a fake copy of the Acme Corp org (the
GitHub, directory and Kubernetes fakes from `internal/fakes`). Bearing reads
them with its real adapters, and you ask it the five questions of the MVP and
see the evidence behind each answer. Then you change the org, ask again, and
see what changed.

Everything here is for demos: the certificates, tokens and settings are made
up and shared by every copy of this repository
([threat model](security/threat-model.md)). Never point this setup at a real
GitHub, cluster or directory.

You need Docker with Compose, and Go 1.27.3 or later to build the `bearing`
command (`make build` writes `bin/bearing`).

## Start it

```sh
cp .env.example .env              # demo values, not secrets
docker compose up -d --build
docker compose cp fakes:/certs/ca.pem demo-ca.pem   # the demo CA, to trust the server
make build
export BEARING_SERVER=https://localhost:8443
export BEARING_TOKEN=$(grep '^BEARING_API_TOKEN=' .env | cut -d= -f2-)
```

`bearing` (the server) listens on `https://localhost:8443`. The fakes'
control endpoint, which plays the org's story, is on `http://127.0.0.1:9090`;
it has no authentication, which is why Compose publishes it on loopback only.
It also refuses a request without the header `X-Bearing-Demo: 1` and any Host
but loopback (`C-DEMO-1` in the [threat model](security/threat-model.md)).

Each source syncs once a minute. Ask for the first syncs now rather than
wait:

```sh
for s in github-acme directory kubernetes-acme; do
  bin/bearing source sync $s --server-ca demo-ca.pem
done
```

The sync runs in the background; the answers below fill in over a few
seconds. (`docker compose logs bearing` shows the server working.)

## The five questions

Output is trimmed (times, IDs and some evidence lines are dropped) and yours
will differ in those.

**1. Which team owns payments-api?**

```sh
bin/bearing owner github:repo/acme/payments-api --server-ca demo-ca.pem
```

```
owners of payments-api [Repository …] github:repo/acme/payments-api
approves_changes payments [Team …] (file .github/CODEOWNERS, line 2, pattern *): asserted 100% valid from …
  github-acme: observed …
    evidence https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS
```

The answer is a fact from CODEOWNERS, with the file, the line and the
observation it came from. `payments` is a GitHub team the directory also
knows as a group, joined by a rule in the settings.

**2. Which services use Go below 1.22?**

```sh
bin/bearing runtimes --language go --below 1.22 --server-ca demo-ca.pem
```

```
go below 1.22

below
  payments-api [Repository …] github:repo/acme/payments-api: 1.21 (go.mod)

1 at or above
```

The evidence is the `go.mod` the adapter read. A repository whose version
cannot be read (the handbook's `lts/hydrogen`) is listed apart, not guessed.

**3. What changed?** At the start only the first sync's facts exist, so play
the org's story first (below) and ask there.

**4. What does billing-api depend on, and how?**

```sh
bin/bearing related kubernetes:workload/k3d-acme/billing/deployment/billing-api \
  --predicate depends_on --direction out --server-ca demo-ca.pem
```

```
depth 1
  billing-api … depends_on postgres … (postgres, port 5432, service billing/postgres, via env:DATABASE_URL): asserted 90%
    kubernetes-acme: observed …
depth 1
  billing-api … depends_on ledger … (http, port 8080, service billing/ledger, via env:LEDGER_URL): asserted 90%
    kubernetes-acme: observed …
```

The how (protocol, port, the Service, and the environment variable that
names it) is kept with the fact. The confidence is below 100% because the
adapter inferred the dependency from a variable's value.

**5. What depends on postgres?**

```sh
bin/bearing related kubernetes:workload/k3d-acme/billing/statefulset/postgres \
  --predicate depends_on --direction in --server-ca demo-ca.pem
```

```
depth 1
  billing-api … depends_on postgres … (postgres, port 5432, service billing/postgres, via env:DATABASE_URL): asserted 90%
```

## Change the org and ask again

The fakes play the org's story on request. Move payments-api's CODEOWNERS
from the payments team to the platform team (this also plays the repository
rename that comes first, and sends GitHub's signed webhook deliveries to
Bearing):

```sh
curl -X POST -H 'X-Bearing-Demo: 1' 'http://127.0.0.1:9090/demo/advance?to=codeowners-move'
curl http://127.0.0.1:9090/demo/steps        # every step, and which are played
bin/bearing source sync github-acme --server-ca demo-ca.pem   # don't wait for the webhook
```

Other steps: `team-rename`, `team-delete`, `membership-end`, `replicas`,
`image`, `team-label`, `annotation`, `dependency` and `workload-delete`; or
`to=2026-10-06T10:00:00Z` for everything up to a time. The story only moves
forward; `docker compose down -v` starts over.

Question 1 again. The old name still finds the repository, which was renamed
to `payments`:

```sh
bin/bearing owner github:repo/acme/payments-api --server-ca demo-ca.pem
```

```
owners of payments [Repository …] github:repo/acme/payments
approves_changes platform [Team …] (file .github/CODEOWNERS, line 2, pattern *): asserted 100% valid from …
  github-acme: observed …
    evidence https://github.com/acme/payments/blob/main/.github/CODEOWNERS
```

Question 3: what changed in the last few minutes?

```sh
bin/bearing changes --since 10m github:repo/acme/payments-api --server-ca demo-ca.pem
```

```
changes of github:repo/acme/payments-api from … to …

… closed
  payments [Repository …] approves_changes payments [Team …] (file .github/CODEOWNERS, line 2, pattern *): … until …
… opened
  payments [Repository …] approves_changes platform [Team …] (file .github/CODEOWNERS, line 2, pattern *): …
… closed
  payments [Repository …] url "https://github.com/acme/payments-api": …
… opened
  payments [Repository …] url "https://github.com/acme/payments": …
```

The ownership change is one fact closed and one opened, each with the
observation behind it; the rename shows as the repository's name and URL
facts.

## Review an identity link

When Bearing is not sure that two records are one thing, it proposes a link
and waits for a person. Where the directory and GitHub agree on a person
(Jane Doe, Mei Chen) the join is already made, so only the doubtful pair is
in the queue:

```sh
bin/bearing links --proposed --server-ca demo-ca.pem
```

```
1 proposed identity links, strongest first

  01a1…  same_as proposed by linked_ids  strength 50%, certainty 50%
    a: Tom Becker [Person …] authentik:user/6c7d8e9f-… (directory)
    b: tbecker [Person …] github:user/tbecker
```

The directory records Tom Becker's GitHub login, but the GitHub user
`tbecker` is a different person with the same login; the two are not the
same, so the right call is to reject the proposal. Only a person decides
links, and only on the server's Unix socket (`api.link_decisions: socket`),
so run the command inside the container, as the service user:

```sh
docker compose exec bearing /usr/local/bin/bearing link reject 01a1… \
  --reason "two different people" --server /run/bearing/api.sock
```

Use the ID your queue shows. Ask `bearing links --proposed` again: the queue
is empty, and the two records still answer as two entities.

## Ask in plain English with Claude Code

`bearing mcp` serves the same answers to an agent (see [mcp.md](mcp.md)). Add
it to Claude Code, with the token read from your shell, never written in
the configuration:

```sh
export BEARING_AGENT_TOKEN=$BEARING_TOKEN   # the demo has one token; a real setup gives an agent its own
claude mcp add bearing -- bin/bearing mcp \
  --server https://localhost:8443 --server-ca "$PWD/demo-ca.pem" --token-env BEARING_AGENT_TOKEN
```

Then ask, for example:

- "Which team owns payments-api, and what changed about it in the last hour?"
- "What does billing-api depend on, and how does it reach each one?"

The agent calls `owners`, `changes` and `depends_on` and answers with the
sources it got back. It has no tool that decides a link or starts a sync.

## What is running

- `fakes`: `bearing-demo-fakes`, the three fakes over HTTPS with a throwaway
  CA it writes to the `certs` volume, and the control endpoint.
- `bearing`: `bearing server --settings /etc/bearing/settings.json` (the
  file is [`demo/settings.json`](../demo/settings.json)), which starts the
  three adapters, syncs them every minute, takes GitHub's webhooks, runs the
  matcher after each sync and serves the API on the Unix socket and on the
  TLS port. Its store is the `data` volume.

Secrets reach the containers as the environment variables in `.env`
(`BEARING_SECRET_*`); the settings name them and never hold them.

Stop it with `docker compose down`, or `docker compose down -v` to forget
everything it learned.

To check that the whole stack works, run `BEARING_TEST_COMPOSE=1 go test
./cmd/bearing-demo-fakes -run Compose`. It builds the image, starts Compose
(ports 8443 and 9090 must be free), asks the five questions and tears it
down. Without Docker, `go test ./cmd/bearing-demo-fakes` runs the same
questions against a server started in-process from the same settings file.
