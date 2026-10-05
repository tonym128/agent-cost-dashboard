# dashd

A long-running cost dashboard for coding agents, in Go.

`dashd` reads the session logs that **pi**, **Claude Code**, **Codex CLI**,
**Gemini CLI**, **Antigravity** and **OpenCode** write, keeps them in a SQLite
database, and serves the dashboard from that database.

The split is the point. A background scanner owns writing; the website only
reads. SQLite runs in WAL mode, so a page load gets a consistent snapshot while a
scan is in progress and neither half waits on the other. Either can also be run
on its own.

## Quick start

```bash
go build -o dashd ./cmd/dashd

# One scan, print what it found, exit. Useful first to see what is there.
./dashd scan

# Serve, and keep scanning every 30 seconds in the background.
./dashd serve-and-scan
# → http://127.0.0.1:8753
```

## Commands

| Command | What it does |
|---|---|
| `serve-and-scan` | Serve the dashboard and scan on a timer (default) |
| `serve` | Serve only; the database is maintained elsewhere |
| `scan` | One pass, then exit — for cron or a systemd timer |
| `reprice` | Recompute every stored cost from the current price table |
| `stats` | Print a summary of the database and the last scan of each source |

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-db` | `~/.local/share/dashd/dashboard.db` | Database path |
| `-addr` | `127.0.0.1:8753` | Listen address; `0.0.0.0` exposes it to the network |
| `-auth-token` | none | Require `Authorization: Bearer <token>`; use it if you expose the dashboard |
| `-interval` | `30s` | How often to scan |
| `-home` | `$HOME` | Where to look for agent logs |
| `-models` | next to the binary, else `./models.json` | OpenRouter price dump |
| `-opencode` | inside `-home` | OpenCode database path |
| `-verbose` | off | Log every request and scan detail |

## How it works

### Calls are stored, not summaries

The unit of storage is **one row per LLM request**. Every figure the dashboard
shows is a rollup of those rows.

That is what makes an old window answerable. A per-session summary cannot answer
"what did I spend last March" once March's logs have been rotated away;
individual calls can. So when a session log disappears, the history is kept and
only the transcript link is withdrawn — the session is marked *orphaned* and the
page says so rather than offering a transcript that cannot open.

```
dashd scan -interval 1h     # hourly, via cron
dashd serve                 # and serve from the same database
```

### Scanning is incremental

A pass stats every known file. Unchanged ones are skipped. For the rest, the
parser reads only what lies beyond the previous pass's cursor:

| Source | Cursor | Incremental by |
|---|---|---|
| pi, Claude, Codex, Gemini | byte offset | seeking to the end of the last complete line |
| Antigravity | row id | `WHERE idx > ?` on its `steps` table |
| OpenCode | max update time | `WHERE time_updated > ?` on messages and parts |

Three details make this correct rather than merely fast:

* **A half-written final line is never consumed.** Agents write their logs live,
  so the last line is routinely incomplete. The cursor stops at the last newline
  and the record is picked up once it is finished.
* **A rewrite is not an append.** A byte offset alone cannot tell "the log grew"
  from "the log was replaced with something longer". Each file's leading bytes
  are fingerprinted and the length of that fingerprint recorded, so an append
  keeps the same fingerprint and a rewrite does not.
* **An idle pass changes nothing.** A pass that reads no new bytes does not
  rewrite the session — which matters, because replacing from an incremental read
  would delete rows the read never returned.

Measured on this machine (282 sessions, ~21,500 calls):

| | |
|---|---|
| First pass, from empty | **~5s** |
| Later pass, nothing changed | **~0.08s** |

That ratio is what lets `-interval` be seconds rather than minutes.

### Pricing

`models.json` (refreshed by `python3 update_models.py`)
is the source of truth for anything it lists. An embedded fallback table covers
the rest, so a missing or unreadable price dump can never leave the dashboard
unable to price anything.

A model with **no** rate is recorded as unpriced rather than at `$0.00`: its
tokens still count, and the page reports the unpriced total. A `$0.00` that
means "we do not know what this costs" must not read as "this was free".

`dashd reprice` recomputes every stored call from the current table, so
refreshing prices corrects history rather than only affecting new calls.

## Layout

```
cmd/dashd/           entry point: flags, wiring, supervision
internal/model/      the types that cross package boundaries
internal/source/     one parser per agent, plus pricing and the protobuf reader
internal/store/      schema, idempotent writes, every read the web layer needs
internal/scan/       source discovery, incremental cursors, the background loop
internal/web/        HTTP handlers, templates, assets
```

The `source` package is the only place that knows what a log format looks like.
Parsers emit `model.Call` and `model.ToolCall`; everything downstream is a rollup.

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

CI runs `go vet`, `gofmt -l` and `go test -race ./...`, and all three must be
clean before a PR. See [CONTRIBUTING.md](CONTRIBUTING.md) for the layout map and
how to add a source.

Around 240 tests, no external Go test dependencies — though `node` is required
for the tests that execute the shipped `dashboard.js` in a sandbox, and they fail
rather than skip when it is absent. The ones that matter most are the ones that
guard properties invisible in a diff:

* **A growing log is extended, not re-counted.** Re-scanning must not duplicate.
* **An idle pass does not erase anything.** Replacing a session from an
  incremental read deletes what the read never returned.
* **A rewritten log drops the calls it removed**; a **deleted log keeps them**.
* **A no-op pass is cheap** — bounded by a timing assertion, because both
  regressions that made it expensive presented as "the scan takes a few seconds".
* **The protobuf reader refuses malformed input** rather than returning a
  plausible wrong number, since its output becomes token counts.
* **Nothing untrusted reaches the page as markup** — model names, project paths
  and titles are all attacker-controlled from the page's point of view. Both
  halves are checked: the Go template by a served page, and the shipped
  `dashboard.js` by executing it.
* **The parsers agree with the previous Python implementation.** There is a
  reference-comparison test, but it reads a developer's own local dump and gemini
  logs, so it skips unless those are present — it is not a guarantee you get from
  a clean checkout.

## Running it

```bash
# as a service
./dashd serve-and-scan -interval 30s

# or split, for a machine where the logs live elsewhere
0 * * * * /usr/local/bin/dashd scan
./dashd serve
```

`GET /healthz` reports the stored totals and the last scan time, which is enough
for a liveness probe or a status bar. It is the one endpoint that does not require
`-auth-token`, so a probe does not have to hold the secret.

### In a container

The image is published as `ghcr.io/tonym128/dashd`. **It binds `127.0.0.1`
inside the container**, which is the same default the binary has. Docker's port
publishing forwards to the container's external interface, not its loopback, so
`-p` on its own reaches nothing:

```bash
# Reading the logs, no dashboard published. This works as-is.
docker run --rm \
  -v "$HOME/.pi:/home/dashd/.pi:ro" \
  -v dashd-data:/var/lib/dashd \
  ghcr.io/tonym128/dashd scan
```

Publishing the dashboard is a deliberate two-part opt-in — an explicit non-loopback
bind **and** a token. Both are required:

```bash
docker run -d -p 127.0.0.1:8753:8753 \
  -v "$HOME/.claude:/home/dashd/.claude:ro" \
  -v dashd-data:/var/lib/dashd \
  ghcr.io/tonym128/dashd \
  serve-and-scan \
    -addr 0.0.0.0:8753 \
    -auth-token "$(openssl rand -hex 32)" \
    -models /usr/local/lib/dashd/models.json \
    -db /var/lib/dashd/dashboard.db
```

Passing a command **replaces the image's default arguments wholesale**, so
`-models` and `-db` have to be repeated: `-models` because `scan` in particular
drops it, and `-db` because it decides where the database lives.

Two traps the `Dockerfile` documents in full: the container's uid has to match
your own, or it cannot read your `0700` agent log directories and the dashboard
reports zero activity rather than failing; and the named volume must be created
owned by you (`docker volume create dashd-data && sudo chown "$(id -u)" dashd-data`)
for the same reason.

## Security

The default bind is `127.0.0.1` and there is no authentication. That is the right
default for a local tool, but `-addr 0.0.0.0` hands the whole dashboard to
anything that can reach the port: project paths, session titles and cost figures,
all read from agent logs you did not write. If you expose it, pass
`-auth-token` and send `Authorization: Bearer <token>`. Note that the token passed
on the command line is visible to any local user through `ps`; see
[SECURITY.md](SECURITY.md) for the threat model and for the alternatives.
