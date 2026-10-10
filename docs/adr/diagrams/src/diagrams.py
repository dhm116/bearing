from ddlib import D


def adr6():
    d = D("adr6-protobuf", 960, 476, "Protobuf is the contract",
          "The Protobuf schemas feed buf, which generates Go code and SDKs and blocks breaking changes; protovalidate checks messages at every way in.")
    d.zone(344, 284, 536, 120, "PROTOVALIDATE AT EVERY WAY IN")
    d.path([(280, 136), (344, 136)], "acc")
    d.path([(496, 120), (544, 120), (544, 56), (624, 56)])
    d.path([(496, 136), (624, 136)])
    d.path([(496, 152), (544, 152), (544, 216), (624, 216)])
    d.path([(752, 248), (752, 284)])
    d.label(588, 44, "GENERATE")
    d.label(588, 124, "CHECK")
    d.label(588, 204, "GENERATE")
    d.raw('<rect class="mask" x="758" y="260" width="80" height="12" rx="2"/><text class="t-lbl" x="798" y="269" text-anchor="middle">RULES CHECKED</text>', "labels")
    d.card(32, 48, 248, "focal", "SOURCE OF TRUTH", "Protobuf schemas", "proto/bearing/…/v1", [
        ("model.proto", "entities · facts"), ("events.proto", "event types"),
        ("adapter.proto", "AdapterService"), ("config.proto", "Adapter · Source"),
        ("audit.proto", "AuditRecord"), ("capability/*.proto", "http · log · kv")], key_w=108)
    d.node(344, 104, 152, 64, "backend", "", "buf", ["lint · breaking", "generate"])
    d.node(624, 24, 256, 64, "store", "GENERATED", "Adapter SDKs · API clients", "Rust · TypeScript · Python")
    d.node(624, 104, 256, 64, "backend", "CI", "Breaking-change gate", "buf breaking against main")
    d.node(624, 184, 256, 64, "store", "GENERATED", "Go code", "gen/go · core, CLI, Go adapters")
    d.node(364, 320, 152, 64, "backend", "", "Event ingest", "webhooks · API")
    d.node(536, 320, 152, 64, "backend", "", "Adapter output", "observations")
    d.node(708, 320, 152, 64, "backend", "", "Config apply", "ADR 10")
    d.legend([("node", "focal", "Source of truth"), ("node", "backend", "Tool or check"),
              ("node", "store", "Generated code"), ("zone", "", "Validation points")])
    return d


def adr7_flow():
    d = D("adr7-flow", 960, 480, "Every change goes through the event log",
          "Webhooks and sync requests are appended to a durable event log; workers run adapters, whose observations go back to the log, and apply changes in one store transaction.")
    d.zone(448, 296, 472, 112, "ONE STORE TRANSACTION")
    d.path([(184, 144), (216, 144), (216, 176), (248, 176)])
    d.path([(184, 240), (216, 240), (216, 208), (248, 208)])
    d.path([(392, 192), (472, 192)], "acc")
    d.path([(648, 180), (760, 180)])
    d.path([(648, 204), (760, 204)], "dash")
    d.path([(856, 160), (856, 104)])
    d.path([(760, 72), (560, 72), (560, 160)])
    d.path([(840, 224), (840, 296)])
    d.label(432, 180, "APPEND", "acc")
    d.label(704, 168, "DELIVER")
    d.label(704, 216, "REPLAY")
    d.raw('<rect class="mask" x="862" y="126" width="28" height="12" rx="2"/><text class="t-lbl" x="876" y="135" text-anchor="middle">RUN</text>', "labels")
    d.label(660, 60, "OBSERVATIONS")
    d.raw('<rect class="mask" x="846" y="254" width="88" height="12" rx="2"/><text class="t-lbl" x="890" y="263" text-anchor="middle">APPLY · ONE TX</text>', "labels")
    d.node(32, 112, 152, 64, "input", "", "Webhooks", "GitHub · PagerDuty")
    d.node(32, 208, 152, 64, "input", "", "Scheduler · CLI", "SyncRequested")
    d.node(248, 160, 144, 64, "backend", "", "Ingest", "validate · assign ID")
    d.node(472, 160, 176, 64, "focal", "WAL", "Event log", "partitioned by entity key", pin=True)
    d.node(760, 160, 160, 64, "backend", "", "Workers", "consumer group")
    d.node(760, 40, 160, 64, "backend", "WASM", "Adapter", "Sync · Handle")
    d.node(464, 328, 136, 64, "store", "", "Graph", "entities · facts")
    d.node(616, 328, 136, 64, "store", "", "Processed IDs", "dedupe by event ID")
    d.node(768, 328, 136, 64, "store", "", "Audit log", "ADR 8")
    d.legend([("node", "focal", "Durable log"), ("node", "backend", "Service"), ("node", "input", "Trigger"),
              ("node", "store", "Store"), ("arrow", "dash", "Replay from an offset")])
    return d


def seq_actor(d, cx, kind, tag, name, sub, y=24, w=160, bottom=300):
    d.raw(f'<line class="life" x1="{cx}" y1="{y+64}" x2="{cx}" y2="{bottom}"/>', "zones")
    d.node(cx - w // 2, y, w, 64, kind, tag, name, sub)


def act(d, cx, y1, y2):
    d.raw(f'<rect class="act" x="{cx-4}" y="{y1}" width="8" height="{y2-y1}"/>', "zones")


def adr7_seq_ingest():
    d = D("adr7-seq-ingest", 960, 376, "A webhook is acknowledged after it is logged",
          "GitHub posts a webhook; ingest appends it to the event log keyed by the delivery ID and only then answers 202 Accepted.")
    for cx, k, t, n, s in [(160, "external", "SOURCE", "GitHub", "webhook sender"),
                           (480, "backend", "SERVICE", "Ingest", "validate · assign ID"),
                           (800, "focal", "WAL", "Event log", "durable append")]:
        seq_actor(d, cx, k, t, n, s, bottom=280)
    act(d, 480, 120, 264)
    act(d, 800, 164, 212)
    d.path([(160, 128), (476, 128)], "link")
    d.label(318, 116, "POST /HOOKS/GITHUB-ACME", "link")
    d.path([(484, 172), (796, 172)])
    d.label(640, 160, "APPEND · KEY = DELIVERY ID")
    d.path([(796, 204), (484, 204)], "dash")
    d.label(640, 192, "OFFSET 1042")
    d.path([(476, 252), (160, 252)], "acc dash")
    d.label(318, 240, "202 ACCEPTED", "acc")
    d.text(480, 312, "The 202 goes out only after the event is safely in the log.", "t-callout", "middle")
    d.legend([("arrow", "link", "HTTP call"), ("arrow", "", "Call"), ("arrow", "dash", "Return"),
              ("arrow", "acc dash", "Headline reply")])
    return d


def adr7_seq_apply():
    d = D("adr7-seq-apply", 960, 536, "Each event is applied exactly once",
          "A worker takes an event from the log, runs the adapter, and in one transaction checks the processed-ID table, writes facts and audit records, and marks the event done before committing its offset.")
    for cx, k, t, n, s in [(120, "store", "WAL", "Event log", "offset 1042"),
                           (360, "backend", "SERVICE", "Worker", "consumer group"),
                           (600, "backend", "WASM", "Adapter", "Handle"),
                           (840, "store", "STORE", "Store", "graph · audit · IDs")]:
        seq_actor(d, cx, k, t, n, s, bottom=448)
    act(d, 360, 112, 432)
    act(d, 600, 148, 196)
    act(d, 840, 216, 396)
    d.raw('<rect class="frame" x="340" y="240" width="524" height="112" rx="4"/>'
          '<rect class="frame-tab" x="340" y="240" width="40" height="16" rx="2"/>'
          '<text class="t-zone" x="360" y="251" text-anchor="middle">OPT</text>'
          '<text class="guard" x="376" y="270">[event ID not seen before]</text>', "zones")
    msgs = [
        ((120, 356), 120, "DELIVER OFFSET 1042", "", 238),
        ((364, 596), 156, "HANDLE(DELIVERY)", "", 480),
        ((596, 364), 188, "OBSERVATIONS · SIG OK", "dash", 480),
        ((364, 836), 224, "BEGIN · SEEN THIS ID?", "", 480),
        ((364, 836), 304, "UPSERT FACTS · AUDIT", "", 480),
        ((364, 836), 336, "MARK ID PROCESSED", "", 480),
        ((364, 836), 384, "COMMIT", "acc", 480),
        ((356, 120), 420, "COMMIT OFFSET 1042", "", 238),
    ]
    for (x1, x2), y, text, kind, lx in msgs:
        d.path([(x1, y), (x2, y)], kind)
        d.label(lx, y - 12, text, "acc" if "acc" in kind else "")
    d.text(480, 476, "A crash before COMMIT means redelivery, and the processed-ID check turns it into a no-op.", "t-callout", "middle")
    d.legend([("arrow", "", "Call"), ("arrow", "dash", "Return"), ("arrow", "acc", "Headline step"),
              ("zone", "", "Runs only for a new event")])
    return d


def adr8_writers():
    d = D("adr8-writers", 960, 392, "The audit log",
          "Workers, people, config apply, policy and executor actions write audit records in the same transaction as their change; a query API and a verify command read them, with optional export and a trace link to OpenTelemetry.")
    d.path([(272, 168), (368, 168)], "acc")
    d.path([(544, 144), (600, 144), (600, 48), (672, 48)])
    d.path([(544, 160), (632, 160), (632, 128), (672, 128)])
    d.path([(544, 176), (632, 176), (632, 208), (672, 208)], "dash")
    d.path([(544, 192), (600, 192), (600, 288), (672, 288)], "dash")
    d.label(320, 156, "SAME TX", "acc")
    d.label(640, 276, "TRACE_ID")
    d.card(32, 96, 240, "input", "WRITERS", "What writes audit records", None, [
        ("Workers", "fact changes"), ("People", "confirm · override"), ("Config apply", "ADR 10"),
        ("Policy", "decisions"), ("Executor", "actions")])
    d.node(368, 128, 176, 80, "focal", "APPEND-ONLY", "Audit log", ["hash-chained", "own retention"], pin=True)
    d.node(672, 16, 256, 64, "backend", "", "Query API", "who changed this, and why?")
    d.node(672, 96, 256, 64, "backend", "", "Chain check", "bearing audit verify")
    d.node(672, 176, 256, 64, "optional", "", "Exporter", "object storage · SIEM")
    d.node(672, 256, 256, 64, "external", "", "OpenTelemetry", "best effort · may expire")
    d.legend([("node", "focal", "Audit log"), ("node", "input", "Writers"), ("node", "backend", "Reader"),
              ("node", "optional", "Optional"), ("node", "external", "External")])
    return d


def adr8_chain():
    d = D("adr8-chain", 960, 408, "A hash chain shows tampering",
          "Each audit record stores the previous record's hash; editing record 2 changes its hash, so record 3's stored prev no longer matches.")
    d.text(64, 36, "INTACT CHAIN", "t-eyebrow")
    d.text(64, 200, "AFTER SOMEONE EDITS #2", "t-eyebrow")
    for y in (48, 212):
        d.path([(272, y + 46), (376, y + 46)])
        d.label(324, y + 34, "HASH → PREV")
    d.path([(584, 94), (688, 94)])
    d.label(636, 82, "HASH → PREV")
    d.path([(584, 258), (688, 258)], "bad")
    d.label(636, 246, "MISMATCH", "bad")
    kw = 40
    d.card(64, 48, 208, "backend", "#1", "Owner set", None, [("prev", "—"), ("hash", "h1")], key_w=kw)
    d.card(376, 48, 208, "backend", "#2", "Owner changed", None, [("prev", "h1"), ("hash", "h2")], key_w=kw)
    d.card(688, 48, 208, "backend", "#3", "Fact retracted", None, [("prev", "h2"), ("hash", "h3")], key_w=kw)
    d.card(64, 212, 208, "backend", "#1", "Owner set", None, [("prev", "—"), ("hash", "h1")], key_w=kw)
    d.card(376, 212, 208, "warn", "#2", "Owner changed (edited)", None, [("prev", "h1"), ("hash", ("h2′", "warn"))], key_w=kw)
    d.card(688, 212, 208, "bad", "#3", "Fact retracted", None, [("prev", ("h2  ≠ h2′", "bad")), ("hash", "h3")], key_w=kw)
    d.text(480, 340, "bearing audit verify walks the chain and reports the first record whose prev doesn't match.", "t-callout", "middle")
    d.legend([("node", "backend", "Record"), ("node", "warn", "Edited record"), ("node", "bad", "Broken link")])
    return d


def adr9_sandbox():
    d = D("adr9-sandbox", 960, 456, "Adapters reach the world through granted capabilities",
          "An adapter module in the WASM sandbox calls one host function; a grant check allows http, local capabilities and kv, or returns a permission error; secrets are injected into http outside the module.")
    d.zone(24, 40, 640, 344, "BEARING WORKER (GO)")
    d.zone(48, 148, 168, 128, "WASM SANDBOX")
    d.path([(200, 220), (272, 220)])
    d.path([(344, 268), (344, 304)], "bad")
    d.path([(416, 196), (452, 196), (452, 112), (488, 112)])
    d.path([(416, 220), (488, 220)])
    d.path([(416, 244), (452, 244), (452, 328), (488, 328)])
    d.path([(640, 112), (752, 112)], "link")
    d.path([(776, 188), (776, 164), (608, 164), (608, 144)], "dash")
    d.path([(640, 328), (752, 328)])
    d.label(236, 208, "CALL")
    d.raw('<rect class="mask" x="350" y="280" width="44" height="12" rx="2"/><text class="t-lbl bad" x="372" y="289" text-anchor="middle">DENIED</text>', "labels")
    d.label(696, 100, "HTTPS", "link")
    d.label(712, 152, "TOKEN")
    d.node(64, 188, 136, 64, "backend", "", "Adapter module", ["github.wasm", "sees bearing_call"])
    d.node(272, 172, 144, 96, "focal", "GATE", "Grant check", ["manifest ∩ Source", "deny by default"], pin=True)
    d.node(280, 304, 128, 64, "bad", "ERROR", "Permission", "back to the module")
    d.node(488, 80, 152, 64, "backend", "", "http", ["allowlist · span", "creds injected"])
    d.node(488, 188, 152, 64, "backend", "", "Local capabilities", ["log · trace · metrics", "config · clock"])
    d.node(488, 296, 152, 64, "backend", "", "kv", "per-Source cache")
    d.node(752, 80, 176, 64, "external", "", "GitHub API", "api.github.com")
    d.node(752, 188, 176, 64, "backend", "", "Secret resolver", ["ADR 10", "token never reaches module"])
    d.node(752, 296, 176, 64, "store", "", "Main store", "kv entries live here")
    d.legend([("node", "focal", "Grant check"), ("node", "backend", "Capability"), ("node", "store", "Store"),
              ("node", "external", "Outside system"), ("node", "bad", "Error"), ("arrow", "dash", "Secret injected")])
    return d


def adr9_topology():
    d = D("adr9-topology", 960, 420, "Same adapter, standalone or distributed",
          "Standalone, capabilities run in-process; distributed, local capabilities stay on the worker while http egress and the kv cache can be remote providers over Connect; a remote adapter service is the fallback runtime.")
    d.zone(24, 40, 240, 276, "STANDALONE · ONE BINARY")
    d.zone(288, 40, 400, 276, "DISTRIBUTED")
    d.zone(712, 40, 224, 276, "REMOTE ADAPTER · FALLBACK")
    d.path([(144, 144), (144, 216)])
    d.path([(442, 144), (442, 180), (360, 180), (360, 216)])
    d.path([(488, 144), (488, 216)], "link")
    d.path([(534, 144), (534, 180), (616, 180), (616, 216)], "link")
    d.path([(824, 144), (824, 216)], "link")
    d.raw('<rect class="mask" x="150" y="174" width="76" height="12" rx="2"/><text class="t-lbl" x="188" y="183" text-anchor="middle">BEARING_CALL</text>', "labels")
    d.raw('<rect class="mask" x="830" y="174" width="52" height="12" rx="2"/><text class="t-lbl link" x="856" y="183" text-anchor="middle">CONNECT</text>', "labels")
    d.node(48, 80, 192, 64, "backend", "WASM", "Adapter", "github.wasm")
    d.node(48, 216, 192, 76, "backend", "", "Capabilities", ["all in-process", "host functions"])
    d.node(396, 80, 184, 64, "backend", "WASM", "Adapter", "on worker N")
    d.node(304, 216, 112, 76, "backend", "", "Local", ["log · trace", "config · clock"])
    d.node(432, 216, 112, 76, "external", "", "HTTP egress", ["remote", "fixed IPs"])
    d.node(560, 216, 112, 76, "external", "", "KV cache", ["remote", "shared"])
    d.node(736, 80, 176, 64, "input", "", "Bearing worker", "calls the adapter")
    d.node(736, 216, 176, 76, "backend", "", "Adapter service", ["same AdapterService", "own process"])
    d.text(480, 348, "The adapter code is the same in all three. Only where each capability runs changes.", "t-callout", "middle")
    d.legend([("node", "backend", "Runs locally"), ("node", "external", "Remote provider"),
              ("arrow", "link", "Connect call"), ("node", "input", "Caller")])
    return d


def adr10_apply():
    d = D("adr10-apply", 960, 428, "Configuration is validated at apply time",
          "Config from Git or the API is validated against proto types, protovalidate rules, the adapter's settings and its capability grant, then stored with an audit record; servers load it and resolve secret references at runtime.")
    d.zone(24, 64, 176, 232, "TWO WAYS IN")
    d.path([(188, 134), (226, 134), (226, 160), (264, 160)])
    d.path([(188, 228), (226, 228), (226, 200), (264, 200)])
    d.path([(360, 244), (360, 292)], "bad")
    d.path([(456, 160), (488, 160), (488, 136), (520, 136)], "acc")
    d.path([(456, 200), (488, 200), (488, 240), (520, 240)])
    d.path([(680, 136), (752, 136)])
    d.path([(840, 168), (840, 232)])
    d.raw('<rect class="mask" x="366" y="262" width="52" height="12" rx="2"/><text class="t-lbl bad" x="392" y="271" text-anchor="middle">INVALID</text>', "labels")
    d.label(716, 124, "LOADS")
    d.raw('<rect class="mask" x="846" y="194" width="52" height="12" rx="2"/><text class="t-lbl" x="872" y="203" text-anchor="middle">RESOLVE</text>', "labels")
    d.node(36, 96, 152, 76, "input", "", "Config in Git", ["config/*.yaml", "bearing diff · apply"])
    d.node(36, 196, 152, 64, "input", "", "API · UI", "same resources")
    d.card(264, 116, 192, "focal", "APPLY", "Validate", None, [
        ("proto types", ""), ("protovalidate rules", ""), ("adapter settings", ""), ("grant ⊆ manifest", "")])
    d.node(280, 292, 160, 64, "bad", "ERROR", "Rejected", "names the field to fix")
    d.node(520, 104, 160, 64, "store", "", "Config tables", "versioned")
    d.node(520, 208, 160, 64, "store", "", "Audit log", "ADR 8")
    d.node(752, 104, 176, 64, "backend", "", "Every server", "scheduler · workers · ingest")
    d.node(752, 232, 176, 76, "backend", "", "Secret resolver", ["env: file: vault: aws-sm:", "never stored or logged"])
    d.legend([("node", "input", "Author"), ("node", "focal", "Validation"), ("node", "store", "Store"),
              ("node", "backend", "Service"), ("node", "bad", "Rejected")])
    return d


def adr10_resources():
    d = D("adr10-resources", 960, 364, "Adapters and Sources",
          "One github Adapter resource, pinned by digest with its capability grant, is used by two Sources that set their own org, token and schedule and may narrow the grant.")
    d.path([(344, 96), (452, 96), (452, 148), (560, 148)])
    d.path([(344, 246), (452, 246), (452, 188), (560, 188)])
    d.label(394, 84, "USES · NARROWS")
    d.label(394, 234, "USES · NARROWS")
    d.card(64, 40, 280, "backend", "SOURCE", "github-acme", None, [
        ("org", "acme"), ("token", "vault:kv/bearing#acme"), ("schedule", "every 6h")], key_w=64)
    d.card(64, 200, 280, "backend", "SOURCE", "github-acme-labs", None, [
        ("org", "acme-labs"), ("kv", "disabled")], key_w=64)
    d.card(560, 104, 320, "focal", "ADAPTER", "github", None, [
        ("module", "sha256:ab12…"), ("grants", "http [api.github.com]"), ("", "kv · log"),
        ("settings", "defaults for every Source")], key_w=64)
    d.text(720, 268, "A Source can narrow the adapter's grant, never widen it.", "t-callout", "middle")
    d.legend([("node", "focal", "Adapter, installed once"), ("node", "backend", "Source, one per org or account")])
    return d


def overview():
    d = D("overview", 960, 480, "Bearing, as proposed",
          "Source systems send webhooks to ingest, which appends to the event log with the scheduler; workers run WASM adapters through capabilities and apply changes to one store, which people reach through the query and config API.")
    d.zone(208, 56, 736, 352, "BEARING SERVER · ONE BINARY OR SCALED OUT")
    d.zone(648, 80, 280, 208, "WORKERS")
    d.path([(176, 136), (248, 136)], "link")
    d.path([(376, 136), (456, 136)], "acc")
    d.path([(376, 248), (416, 248), (416, 168), (456, 168)])
    d.path([(600, 152), (648, 152)])
    d.path([(784, 144), (808, 144)])
    d.path([(864, 112), (864, 32), (104, 32), (104, 104)], "link")
    d.path([(788, 288), (788, 320)])
    d.path([(176, 352), (448, 352)])
    d.path([(600, 352), (656, 352)])
    d.label(212, 124, "WEBHOOKS", "link")
    d.label(416, 124, "APPEND · ACK", "acc")
    d.label(520, 20, "ALLOWED HTTP", "link")
    d.raw('<rect class="mask" x="794" y="298" width="88" height="12" rx="2"/><text class="t-lbl" x="838" y="307" text-anchor="middle">APPLY · ONE TX</text>', "labels")
    d.node(32, 104, 144, 64, "external", "", "Source systems", "GitHub, AWS, PagerDuty")
    d.node(32, 320, 144, 64, "input", "", "People · agents", "CLI · UI · API clients")
    d.node(248, 104, 128, 64, "backend", "", "Ingest", "webhooks · API")
    d.node(248, 216, 128, 64, "backend", "", "Scheduler", "SyncRequested")
    d.node(456, 112, 144, 80, "focal", "ADR 7", "Event log", "durable · WAL", pin=True)
    d.node(664, 112, 120, 64, "backend", "", "WASM adapters", "ADR 9")
    d.node(808, 112, 112, 64, "backend", "", "Capabilities", "http · log · kv")
    d.node(664, 200, 120, 64, "backend", "", "Resolve", ["identities", "score facts"])
    d.node(448, 320, 152, 64, "backend", "", "Query · config API", "Connect · ADR 6")
    d.node(656, 320, 272, 64, "store", "", "Store", ["graph · vectors · audit · config", "PostgreSQL by default · ADR 14"])
    d.legend([("node", "focal", "Event log"), ("node", "backend", "Component"), ("node", "store", "Store"),
              ("node", "external", "Source system"), ("node", "input", "People"), ("arrow", "link", "Outbound HTTP")])
    return d


ALL = [overview, adr6, adr7_flow, adr7_seq_ingest, adr7_seq_apply, adr8_writers, adr8_chain,
       adr9_sandbox, adr9_topology, adr10_apply, adr10_resources]
