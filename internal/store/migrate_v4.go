package store

// The version 4 step: make the next scan re-read every Claude Code log from the
// start.
//
// Claude Code writes one JSONL line per content block of a response, and each
// line repeats the response's full usage. The parser keyed a call by the line's
// uuid, so a response with a thinking block, a text block and a tool call became
// three calls with three copies of the same tokens — on a real log, 169 calls
// for 83 responses and roughly double the true cost. Tool calls had the same
// shape of problem: one row for the tool_use and a second, unnamed one for its
// tool_result, keyed by position in the batch.
//
// The parser now keys a call by the API message id and a tool by its tool_use
// id. Those are new keys, so an incremental read would add rows beside the old
// ones rather than overwrite them. Dropping the scan cursors instead sends each
// log through a read from offset zero, which the scanner commits with
// ReplaceSession: the session's old rows are cleared and rebuilt under the new
// keys in the same transaction.
//
// Only cursors are deleted, never calls. A session whose log has since been
// deleted is not re-read and keeps its rows as they were, which is the
// trade-off: its history stays, inflated, rather than disappearing.
//
// Safe to re-run: on a database with no Claude cursors it deletes nothing.
const v4ResetClaudeCursors = `
DELETE FROM scan_state WHERE agent = 'claude';
`

var v4ResetClaude = migration{
	version: 4,
	name:    "re-read Claude Code logs under per-response call keys",
	apply:   execScript(v4ResetClaudeCursors),
}
