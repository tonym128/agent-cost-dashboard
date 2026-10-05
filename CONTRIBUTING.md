# Contributing

## Build and test

```bash
go build -o dashd ./cmd/dashd

go test ./...
go test -race ./...
go vet ./...
gofmt -l .        # must print nothing
```

CI runs `go vet`, `gofmt -l` and `go test -race ./...`. All three must be clean
before a PR opens. There are no external test dependencies; no database or
network fixture needs provisioning to run the suite.

## Layout

| Path | Holds |
|---|---|
| `cmd/dashd/` | entry point: flags, wiring, supervision |
| `internal/model/` | the types that cross package boundaries |
| `internal/source/` | one parser per agent, plus pricing and the protobuf reader |
| `internal/store/` | schema, idempotent writes, every read the web layer needs |
| `internal/scan/` | source discovery, incremental cursors, the background loop |
| `internal/web/` | HTTP handlers, templates, assets |

The `source` package is the only place that knows what a log format looks like.
Parsers emit `model.Call` and `model.ToolCall`; everything downstream is a rollup.
Keep it that way — a parser that reaches into `store` or `web` to save itself is a
bug.

## Testing philosophy

Tests here assert **invariants**, not golden output. The regressions that cost
real time were re-counted rows, passes that quietly deleted history, and a
protobuf reader that returned a plausible wrong number — none of which shows up
in a diff, and none of which a snapshot of pretty-printed HTML would catch.

So: assert the property ("rescanning does not duplicate", "an idle pass does not
write", "a deleted log keeps its rows"), and let the test fail loudly with a
count or a diff of the two sides when it breaks. Do not add golden-file tests for
whole page renders.

Two tests do guard performance, because both regressions that made scanning
expensive presented identically as "the scan takes a few seconds": a no-op pass
is bounded by a timing assertion, and incremental resume is bounded by bytes
read.

## Adding a new agent source

1. A parser in `internal/source/` that yields `model.Call` / `model.ToolCall`,
   with its own incremental cursor.
2. Registration in `internal/scan/` so discovery finds it and the background
   loop drives it.
3. A fixture in `internal/source/testdata/` — a small log, or the leading slice of
   one, containing at least one complete record and ideally a truncated final
   line.
4. A test that exercises the cursor: append to the fixture and assert the new
   records appear exactly once, and that a rewrite drops what it removed.