# Bearing graph (spike)

A live picture of what Bearing knows: every entity as a card, the facts
between them as lines, and identity links Bearing is unsure of as dotted
lines. It updates while the demo story plays.

Spike only (`spike/graph-svelte`), never merged: `main` holds Go and shell
only.

Built with SvelteKit 3 on Bun, daisyUI 5 (Bearing's brand colours as the
`bearing-dark` and `bearing-light` themes), Svelte Flow 2 for the canvas and
ELK for the layout.

## Run it

Start the demo (see [docs/demo.md](../../docs/demo.md)), from the repository
root:

```sh
cp .env.example .env
docker compose up -d --build
docker compose cp fakes:/certs/ca.pem web/graph/demo-ca.pem
```

Then the app:

```sh
cd web/graph
bun install
BEARING_TOKEN=$(grep '^BEARING_API_TOKEN=' ../../.env | cut -d= -f2-) \
BEARING_CA=$PWD/demo-ca.pem bun --bun vite dev --port 5180
```

Open http://localhost:5180. **Sync now** reads the three Acme sources;
**Next step** plays one change of the story and syncs; **Auto** plays one
every six seconds.

## How it works

```
browser ──EventSource──▶ /api/stream ──poll 1.5 s──▶ Bearing GET /v1/graph
        ──fetch────────▶ /api/demo   ──────────────▶ fakes /demo/advance, Bearing sync
```

- `GET /v1/graph` (spike route in `pkg/server`) returns `pkg/graphview`'s
  picture, the one `bearing graph` draws.
- The SvelteKit server holds the API token and polls; the browser gets the
  picture over server-sent events only when it changes.
- The page diffs each new picture against the last for the "What changed"
  feed and to glow what is new (green) or renamed (amber).
- Columns follow kinds: people, teams, repositories, components,
  environments. Click a card to focus it and its neighbours and read its
  records, attributes, facts with their evidence, and identity links.

| Variable | Default |
| --- | --- |
| `BEARING_SERVER` | `https://localhost:8443` |
| `BEARING_TOKEN` | none: required |
| `BEARING_CA` | none (system roots) |
| `BEARING_DEMO_CONTROL` | `http://127.0.0.1:9090` |
| `BEARING_SOURCES` | `github-acme,directory,kubernetes-acme` |
