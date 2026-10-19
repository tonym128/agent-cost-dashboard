package store

import "database/sql"

// The version 5 step: store how much of a call's cache writes had a one-hour
// lifetime.
//
// Anthropic bills a one-hour cache write at twice the input rate, against 1.25x
// for a five-minute one, and every cache write Claude Code makes is the
// one-hour kind. Without the split stored, the scan could price it correctly
// but a reprice, which works from the stored columns, would undo it.
//
// The column is added by this step alone, for fresh and existing databases
// alike, rather than also written into `schema`: ALTER TABLE records a column in
// the table's stored definition differently from a CREATE TABLE that lists it,
// and the two paths must end up with the same schema text.
//
// Claude's scan cursors are dropped again, as in v4, so every Claude log is
// re-read from the start and the new column is filled for calls already
// stored. No other agent reports the split, so no other agent is re-read.
//
// Safe to re-run: the column is added only if missing, and on a database with
// no Claude cursors the delete removes nothing.
var v5CacheWrite1h = migration{
	version: 5,
	name:    "store one-hour cache writes; re-read Claude Code logs to fill them",
	apply: func(tx *sql.Tx) error {
		if err := addColumn(tx, "call", "cache_write_1h_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		return execScript(v4ResetClaudeCursors)(tx)
	},
}
