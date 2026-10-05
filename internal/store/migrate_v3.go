package store

// The version 3 schema step: drop the four single-column indexes that version 2
// made redundant.
//
// The reasoning, and the numbers behind it, are below. The short version: each
// of these four is a strict prefix of a covering index that already exists, so
// no plan can use one that cannot use the other — while every one of them costs
// an index entry per ingested call, forever.
//
// Call_day  is a prefix of call_day_model_cost(day, model, cost_usd).
// Call_project is a prefix of call_project_model_cost(project, model, cost_usd,
//
//	total_tokens, output_tokens, llm_seconds).
//
// Call_session is a prefix of call_session_ts(session_uid, ts, cost_usd).
// Call_model is a prefix of call_model_roll(model, cost_usd, total_tokens, …).
//
// `call` is WITHOUT ROWID, so every index entry carries the whole primary key
// (session_uid, call_key). Four redundant entries per call is not a rounding
// error to be dismissed: it is the single largest remaining write cost in the
// ingestion path, which runs every 30 seconds by default.
//
// This step is destructive and one-way, which is worth stating plainly. SQLite
// cannot un-drop an index, so there is no migration that puts these four back —
// the reverse is a *new* step, version 4, and it would rebuild them from the
// rows still in `call`, so the data is never at risk, only the time. A binary
// that predates v3 refuses to open a v3 database rather than guessing, which
// is the intended behaviour and is what makes the asymmetry safe: a downgraded
// dashd refuses to start rather than silently serving a shape it does not
// understand. Anyone who needs the old shape back writes the four CREATE INDEX
// statements, or restores a backup.
//
// The IF EXISTS guards are there because this step costs nothing to be safe to
// re-run: a database already trimmed by an interrupted run, or one that never
// had the indexes, opens unchanged.
const v3DropRedundantIndexes = `
DROP INDEX IF EXISTS call_day;
DROP INDEX IF EXISTS call_project;
DROP INDEX IF EXISTS call_session;
DROP INDEX IF EXISTS call_model;
`

// v3DropPrefixIndexes is the version 3 migration, as a value.
//
// It is a value rather than an entry written inline in the list in store.go so
// that the DDL and the step that applies it cannot drift apart, and so that the
// step is testable on its own. Adding it to the list is one line — see the
// report on this change: `migrations` is declared in store.go and that file is
// owned elsewhere, so the append itself has not been made here.
var v3DropPrefixIndexes = migration{
	version: 3,
	name:    "drop the indexes version 2 made redundant",
	apply:   execScript(v3DropRedundantIndexes),
}
