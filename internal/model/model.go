// Package model holds the types that cross package boundaries: what a parser
// emits, what the store persists, and what the web layer serves.
//
// The central type is Call, one row per LLM request. Everything the dashboard
// displays is a rollup of Calls, which is what makes an arbitrary historical
// window answerable from the database alone once the session logs that produced
// it have been rotated away.
package model

import "time"

// Agent identifies which tool wrote a session log. The values match the
// directory names and agent commands the Python implementation used, so an
// existing filter query keeps working.
const (
	AgentPi       = "pi"
	AgentClaude   = "claude"
	AgentCodex    = "codex"
	AgentGemini   = "gemini"
	AgentAgy      = "agy"
	AgentOpencode = "opencode"
)

// Call is a single LLM request extracted from a session log.
//
// CallKey makes ingestion idempotent: re-scanning a log rewrites the same
// (SessionUID, CallKey) rows rather than appending duplicates. It must therefore
// be stable across scans for the same call — the parser's own id when the log
// provides one, otherwise the call's ordinal within the session.
type Call struct {
	SessionUID string
	Agent      string
	Project    string
	Model      string
	CallKey    string

	// Time is when the call completed. Zero when the log carries no timestamp;
	// such calls still count toward totals but cannot appear in a time window.
	Time time.Time
	// Day is Time formatted as local YYYY-MM-DD, used for cheap grouping.
	Day string

	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	TotalTokens      int

	// LLMSeconds is wall time attributed to the call, used for response-time
	// and throughput figures. Zero when the log has no usable timing.
	LLMSeconds float64

	// CostUSD is priced at scan time. `dashd reprice` recomputes it from the
	// token columns after a pricing refresh, so old calls are not stuck at
	// whatever the rates were when they were first seen.
	CostUSD float64
	// Priced is false when no rate was found for the model. The call is still
	// stored and still counts tokens; it must not be silently reported as free.
	Priced bool
}

// ToolCall is a single tool invocation.
type ToolCall struct {
	SessionUID string
	Agent      string
	Project    string
	Tool       string
	CallKey    string
	Time       time.Time
	Seconds    float64
	IsError    bool
}

// Session is the per-session row behind the sessions table. It is derived data:
// every field is a rollup of that session's calls and tool calls, recomputed on
// each scan rather than incrementally maintained, which keeps it correct when a
// log is rewritten rather than only appended to.
type Session struct {
	UID     string
	Agent   string
	Project string
	Path    string
	Title   string

	FirstTS time.Time
	LastTS  time.Time
	// WallSecs is last minus first: how long the session was open, which
	// includes idle gaps and is deliberately distinct from summed LLM time.
	WallSecs float64

	Calls           int
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	// CacheWriteTokens is reported separately because several agents bill it
	// at a distinct rate and it is otherwise invisible in the totals.
	CacheWriteTokens int
	ReasoningTokens  int
	TotalTokens      int

	CostUSD     float64
	LLMSeconds  float64
	ToolSeconds float64
	ToolCalls   int
	ToolErrors  int
}

// SessionWrite is everything a parser produced for one session log: the calls,
// the tool calls, and the identity of the session they belong to.
//
// It is passed whole to the store and committed in one transaction, so a session
// is either fully ingested or not at all.
type SessionWrite struct {
	Session
	Path string
	// FirstTS and LastTS are fallbacks used when no call carried a timestamp,
	// so a session is still ordered sensibly in the list.
	FirstTS   time.Time
	LastTS    time.Time
	Calls     []Call
	ToolCalls []ToolCall
}

// ScanStatus is what one pass over one source produced. It is recorded so a
// stale dashboard can be explained rather than guessed at.
type ScanStatus struct {
	Agent string
	// LastScanAt is when the pass finished.
	LastScanAt time.Time
	// LastFullAt is when a pass last actually re-read file contents, as opposed
	// to finding everything unchanged.
	LastFullAt time.Time
	LastFull   bool

	FilesSeen     int
	FilesChanged  int
	CallsIngested int
	Error         string
}

// ScanState tracks how much of one source file has already been consumed.
//
// Offset is a resume cursor whose meaning depends on the source: for a JSONL log
// it is a byte position just past the last complete line; for a SQLite source it
// is that source's own change cursor (a row id or a maximum update time).
//
// A half-written final line is normal for a log an agent is still writing, so a
// JSONL offset never advances past one.
type ScanState struct {
	Path    string
	Agent   string
	Size    int64
	ModTime time.Time
	Offset  int64
	// PrefixHash fingerprints the first PrefixLen bytes of the file. Offset alone
	// cannot distinguish "the log grew" from "the log was rewritten in place
	// and happens to be at least as long", and treating the second case as an
	// append would silently keep calls that the file no longer contains.
	//
	// PrefixLen is recorded alongside the hash and is what makes this work for
	// small files: hashing the whole of a file shorter than the window means any
	// append changes the hash, so every growing log would look rewritten.
	PrefixHash string
	PrefixLen  int64
	// Cursor names what Offset means, so a source whose layout changes cannot
	// resume from an offset interpreted the other way.
	Cursor     string
	SessionUID string
	// Complete marks a source known not to change again, so it is never
	// revisited. Several of these logs are immutable once written.
	Complete bool
	Scanned  time.Time
}

// Cursor kinds recorded in ScanState.Cursor.
const (
	CursorBytes   = "bytes"    // JSONL: byte offset into an append-only log
	CursorRowID   = "rowid"    // SQLite: highest row id consumed
	CursorTimeMax = "time_max" // SQLite: highest update timestamp consumed
)

// CursorPrefixBytes is how much of a file's head is fingerprinted.
const CursorPrefixBytes = 4096
