package source

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
	_ "modernc.org/sqlite"
)

// openReadOnly opens a source database read-only.
//
// The URI is percent-encoded rather than interpolated raw: a path containing "?"
// or "#" would otherwise silently change the URI's meaning and open, or fail to
// open, something else.
// OpenReadOnly opens a source database read-only, for callers outside this
// package that need to enumerate what it contains.
func OpenReadOnly(path string) (*sql.DB, error) { return openReadOnly(path) }

func openReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// ---------------------------------------------------------------- Antigravity

// AntigravityParser reads one conversation database.
//
// Antigravity records each conversation as a SQLite file whose `steps` table
// carries protobuf blobs. The `idx` column is monotonic, which makes it a
// natural resume cursor: a later scan reads only the steps that appeared since,
// rather than re-decoding a few hundred blobs every time.
type AntigravityParser struct{}

// Step type and status constants, as they appear in the `steps` table.
const (
	agyStepLLM1  = 15
	agyStepLLM2  = 23
	agyStepTool  = 132
	agyStepError = 7
	// Field numbers inside the step metadata and usage sub-messages.
	agyMetaStartTS    = 1
	agyMetaEndTS      = 8
	agyMetaToolDesc   = 4
	agyMetaUsage      = 9
	agyUsageModel     = 1
	agyUsageInput     = 2
	agyUsageOutput    = 3
	agyUsageCacheRd   = 5
	agyUsageReasoning = 9
)

// agyModelNames maps the model ids Antigravity uses to the display labels the
// rest of the system refers to them by.
//
// An id missing from this map is a model released after this build. It is named
// plainly and counted as unpriced rather than being guessed at, so the cost
// shows as unknown instead of silently as zero.
var agyModelNames = map[int32]string{
	342:  "Gemini 3.1 Flash Lite",
	1016: "Gemini 3.8 Flash (Low)",
	1020: "Gemini 3.8 Flash (Medium)",
	1021: "Gemini 3.8 Flash (High)",
	1026: "Claude Opus 4.6 (Thinking)",
	1035: "Claude Sonnet 4.6 (Thinking)",
	1036: "Claude Sonnet 4.6",
	1037: "Claude Opus 4.6",
	1050: "Gemini 3.8 Pro (Low)",
	1071: "Gemini 3.8 Pro (Medium)",
	1072: "Gemini 3.8 Pro (High)",
	1073: "Gemini 3.8 Pro (Thinking)",
	1196: "GPT-OSS 120B (Medium)",
	1266: "GPT-OSS 120B",
	1298: "Gemini 3.1 Pro (Low)",
	1299: "Gemini 3.1 Pro (Medium)",
	1300: "Gemini 3.1 Pro (High)",
	1318: "Gemini 3.1 Flash (Low)",
	1319: "Gemini 3.8 Flash (Medium)",
	1320: "Gemini 3.1 Flash (Medium)",
	1322: "Gemini 3.1 Flash (High)",
}

func NewAntigravityParser() Parser { return &AntigravityParser{} }

func (p *AntigravityParser) Agent() string { return model.AgentAgy }

// SessionUID is the database file's stem: Antigravity names each conversation
// database after the conversation.
func (p *AntigravityParser) SessionUID(path string) string { return stem(path) }

func (p *AntigravityParser) Parse(path string, prev model.ScanState, pricer *Pricer) (model.SessionWrite, model.ScanState, error) {
	uid := stem(path)
	next := model.ScanState{
		Path: path, Agent: model.AgentAgy, Cursor: model.CursorRowID,
		SessionUID: uid, ModTime: modTime(path), Offset: prev.Offset,
		// Size is half of the scanner's "has this changed" check. Left at zero
		// the conversation looks changed on every pass and its steps are
		// re-decoded every time.
		Size: fileSize(path),
	}

	db, err := openReadOnly(path)
	if err != nil {
		return model.SessionWrite{}, next, fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()

	project := agyReadWorkspace(db)

	b := newSessionBuilder(uid, model.AgentAgy, project)
	// Only steps past the cursor. A cursor of zero reads the whole conversation;
	// otherwise this is an increment and the caller appends it.
	rows, err := db.Query(`
		SELECT idx, step_type, status, metadata
		FROM steps WHERE idx > ? ORDER BY idx`, prev.Offset)
	if err != nil {
		return model.SessionWrite{}, next, fmt.Errorf("read steps of %s: %w", path, err)
	}
	defer rows.Close()

	var maxIdx int64 = prev.Offset
	for rows.Next() {
		var idx int64
		var stepType int
		var status int
		var metadata []byte
		if err := rows.Scan(&idx, &stepType, &status, &metadata); err != nil {
			return model.SessionWrite{}, next, err
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		p.consumeStep(b, idx, stepType, status, metadata, pricer)
	}
	if err := rows.Err(); err != nil {
		return model.SessionWrite{}, next, err
	}
	next.Offset = maxIdx
	return b.build(path), next, nil
}

func (p *AntigravityParser) consumeStep(b *sessionBuilder, idx int64, stepType, status int, metadata []byte, pricer *Pricer) {
	if len(metadata) == 0 {
		return
	}
	// One decode of the metadata serves every field this step needs. Asking for
	// the fields one at a time meant re-scanning and re-copying the whole blob
	// four times per step, which was the single most expensive thing in a scan.
	fields := ProtoFields(metadata)

	startSec, hasStart := ProtoTimestamp(fieldBytes(fields, agyMetaStartTS))
	endSec, hasEnd := ProtoTimestamp(fieldBytes(fields, agyMetaEndTS))
	stepSeconds := 0.0
	if hasStart && hasEnd && endSec > startSec {
		stepSeconds = float64(endSec - startSec)
		// A step cannot plausibly have taken ten minutes; anything larger is a
		// clock artefact or an idle period, not a response.
		if stepSeconds > 600 {
			stepSeconds = 0
		}
	}

	var stepStart time.Time
	if hasStart {
		stepStart = time.Unix(startSec, 0).UTC()
		b.observeTime(tsStr(stepStart))
	}

	switch stepType {
	case agyStepLLM1, agyStepLLM2:
		usageBlob := fieldBytes(fields, agyMetaUsage)
		if len(usageBlob) == 0 {
			return
		}
		u := ProtoVarints(usageBlob)
		if len(u) == 0 {
			// The usage sub-message decoded to no fields at all: a varint that
			// never terminates, or a body truncated inside its own declared
			// length. That is a corrupt blob rather than a call that spent
			// nothing, and it must not become a row — a zero-token call would
			// pad the call count with a call that never happened. This is the
			// same truncation the length check above handles, one level down.
			return
		}

		modelID := int32(u[agyUsageModel])
		modelName, known := agyModelNames[modelID]
		if !known {
			modelName = fmt.Sprintf("Antigravity Model %d", modelID)
		}
		// Antigravity's input and cache-read counters are disjoint buckets
		// rather than a total and a part of it, so they are summed and never
		// subtracted — the opposite of the Gemini convention above.
		input := clampNonNeg(u[agyUsageInput])
		generated := clampNonNeg(u[agyUsageOutput])
		cacheRead := clampNonNeg(u[agyUsageCacheRd])
		reasoning := clampNonNeg(u[agyUsageReasoning])
		visible, reasoningOut := visibleOutput(generated, reasoning)

		c := model.Call{
			Model:           Hyphenate(modelName),
			Time:            stepStart,
			InputTokens:     int(input),
			OutputTokens:    int(visible),
			CacheReadTokens: int(cacheRead),
			ReasoningTokens: int(reasoningOut),
			LLMSeconds:      stepSeconds,
			CallKey:         fmt.Sprintf("step%d", idx),
		}
		// The whole generated count is billed at the output rate, not just the
		// itemised non-reasoning remainder above.
		priceGenerated(pricer, &c, int(generated))
		b.addCall(c)

	case agyStepTool:
		desc := fieldBytes(fields, agyMetaToolDesc)
		if len(desc) == 0 {
			return
		}
		name := ProtoString(desc, 2)
		if name == "" {
			return
		}
		b.addTool(model.ToolCall{
			Tool:    name,
			Time:    stepStart,
			Seconds: stepSeconds,
			IsError: status == agyStepError,
		})
	}
}

// agyReadWorkspace finds the conversation's working directory.
//
// Which submessage holds it depends on which front-end wrote the conversation,
// so three known paths are tried and the most frequent answer wins. The
// alternatives are a cache miss, not a correctness question, so a database that
// has none of them simply reports an unknown workspace.
func agyReadWorkspace(db *sql.DB) string {
	paths := [][]int32{
		{28, 2},
		{140, 1, 2},
		{19, 12, 1, 42, 11, 1},
	}
	counts := map[string]int{}

	rows, err := db.Query("SELECT step_payload FROM steps WHERE step_payload IS NOT NULL")
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil || len(blob) == 0 {
			continue
		}
		for _, path := range paths {
			raw := blob
			ok := true
			for _, field := range path {
				f, found := ProtoField(raw, field, 2)
				if !found {
					ok = false
					break
				}
				raw = f.Bytes
			}
			if !ok || len(raw) == 0 {
				continue
			}
			v := string(raw)
			if len(v) > 0 && v[0] == '/' {
				counts[v]++
			}
		}
	}
	if len(counts) == 0 {
		return ""
	}
	best, bestCount := "", -1
	for path, n := range counts {
		if n > bestCount || (n == bestCount && path < best) {
			best, bestCount = path, n
		}
	}
	return best
}

func fieldBytes(fields map[int32]PBField, num int32) []byte {
	if f, ok := fields[num]; ok && f.Wire == 2 {
		return f.Bytes
	}
	return nil
}

func clampNonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// ---------------------------------------------------------------- OpenCode

// OpenCodeParser reads OpenCode's single shared database.
//
// One database holds every session in every project, so the file is not the unit
// of ingestion — the session row is. Each session is scanned independently, keyed
// on the highest message time already consumed, so adding a message to one
// session does not re-read the other four hundred.
//
// Two schemas are in play, because OpenCode 2 migrated its storage rather than
// replacing it in place: sessions moved from `session` to `session_v2`, messages
// from `message` to `session_message`, and a message's role moved out of the JSON
// blob and into a column. The migration *copies* rather than replaces, so an
// upgraded database holds both sets at once, while a database written by a 1.x
// release holds only the old ones. Reading whichever tables are present is what
// makes both readable; reading only the old ones is what left every OpenCode 2
// session — and its tool calls — invisible to the dashboard, with no error to
// show for it: the old tables still exist, they just no longer hold the new work.
type OpenCodeParser struct {
	DBPath string
}

// The tables the OpenCode parser reads, most current first. The order is the
// precedence used when the same row is found in two: OpenCode 2's copy is the
// one it now writes, so it wins.
var (
	openCodeSessionTables = []string{"session_v2", "session"}
	openCodeMessageTables = []string{"session_message", "message"}
	openCodePartTable     = "part"
)

// openCodeLayout is which of those tables a particular database actually has.
//
// It is discovered rather than assumed. A 1.x database has no `session_v2` at
// all, and a build of ours that only knows the old tables would otherwise fail
// against every upgraded install — or, worse, quietly read a stale table and
// report a confident total of whatever the migration left behind.
type openCodeLayout struct {
	// sessions and messages list the tables present, most current first.
	sessions []string
	messages []string
	hasParts bool
}

// openCodeLayoutOf reports which tables the database holds.
//
// A database with none of them is an error rather than an empty result: that is
// the shape of a truncated file or a database OpenCode has rewritten under us,
// and reporting "no sessions" for it would be indistinguishable from a user who
// has not used the tool.
func openCodeLayoutOf(db *sql.DB) (openCodeLayout, error) {
	present := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return openCodeLayout{}, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return openCodeLayout{}, err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return openCodeLayout{}, err
	}

	var l openCodeLayout
	for _, t := range openCodeSessionTables {
		if present[t] {
			l.sessions = append(l.sessions, t)
		}
	}
	for _, t := range openCodeMessageTables {
		if present[t] {
			l.messages = append(l.messages, t)
		}
	}
	l.hasParts = present[openCodePartTable]

	switch {
	case len(l.sessions) == 0:
		return l, fmt.Errorf("no OpenCode session table: this database has none of %s",
			strings.Join(openCodeSessionTables, " or "))
	case len(l.messages) == 0:
		return l, fmt.Errorf("no OpenCode message table: this database has none of %s",
			strings.Join(openCodeMessageTables, " or "))
	}
	return l, nil
}

// NewOpenCodeParser returns the concrete type rather than the Parser interface,
// so a caller scanning a whole pass can reach the batched ParseAll.
func NewOpenCodeParser(dbPath string) *OpenCodeParser { return &OpenCodeParser{DBPath: dbPath} }

func (p *OpenCodeParser) Agent() string { return model.AgentOpencode }

// SessionUID is the session id, which is the primary key of the row this file
// holds; the database as a whole has no single session.
func (p *OpenCodeParser) SessionUID(path string) string { return stem(path) }

// Parse ingests one session, opening the database for the duration.
//
// Prefer ParseAll for a whole pass: this opens the shared database per session,
// which is fine for a single lookup and wasteful across a hundred.
func (p *OpenCodeParser) Parse(sessionID string, prev model.ScanState, pricer *Pricer) (model.SessionWrite, model.ScanState, error) {
	db, err := openReadOnly(p.DBPath)
	if err != nil {
		return model.SessionWrite{}, model.ScanState{
			Path:       "opencode:" + sessionID,
			Agent:      model.AgentOpencode,
			Cursor:     model.CursorTimeMax,
			SessionUID: sessionID,
			Size:       fileSize(p.DBPath),
		}, fmt.Errorf("open %s: %w", p.DBPath, err)
	}
	defer db.Close()
	layout, err := openCodeLayoutOf(db)
	if err != nil {
		return model.SessionWrite{}, openCodeRetryState(p.DBPath, sessionID), err
	}
	return p.parseWith(db, layout, sessionID, prev, pricer)
}

// openCodeRetryState is the cursor returned when a database cannot be opened or
// understood.
//
// It names the session and carries the time cursor, so the next pass retries
// from the start rather than treating the session as read to the end: a scan that
// failed halfway through a schema it did not recognise must not leave a cursor
// that says the work is done.
func openCodeRetryState(dbPath, sessionID string) model.ScanState {
	return model.ScanState{
		Path:       "opencode:" + sessionID,
		Agent:      model.AgentOpencode,
		Cursor:     model.CursorTimeMax,
		SessionUID: sessionID,
		Size:       fileSize(dbPath),
	}
}

// ParseAll ingests many sessions over one connection.
//
// Every OpenCode session lives in the same file, so a pass re-opened it once
// per session: a no-op pass over a hundred sessions spent its whole time
// opening databases rather than noticing nothing had changed.
func (p *OpenCodeParser) ParseAll(ids []string, prev map[string]model.ScanState, pricer *Pricer) (map[string]model.SessionWrite, map[string]model.ScanState, error) {
	sessions := map[string]model.SessionWrite{}
	states := map[string]model.ScanState{}
	if len(ids) == 0 {
		return sessions, states, nil
	}
	db, err := openReadOnly(p.DBPath)
	if err != nil {
		return sessions, states, fmt.Errorf("open %s: %w", p.DBPath, err)
	}
	defer db.Close()
	// Discovered once for the whole batch: it is one query against
	// sqlite_master, and a pass over a hundred sessions should not repeat it a
	// hundred times.
	layout, err := openCodeLayoutOf(db)
	if err != nil {
		return sessions, states, err
	}
	for _, id := range ids {
		sess, next, err := p.parseWith(db, layout, id, prev[id], pricer)
		if err != nil {
			return sessions, states, err
		}
		sessions[id] = sess
		states[id] = next
	}
	return sessions, states, nil
}

// OpenCodeSessionIDs lists the ids of every top-level session in an OpenCode
// database, from every session table it holds.
//
// Child sessions are excluded: a subagent's messages belong to its parent's
// conversation, and ingesting them as sessions of their own would count the same
// calls twice.
func OpenCodeSessionIDs(db *sql.DB) ([]string, error) {
	layout, err := openCodeLayoutOf(db)
	if err != nil {
		return nil, err
	}
	// A migrated session is present in both tables under one id, so the result is
	// deduplicated here rather than in the caller.
	seen := map[string]bool{}
	var out []string
	for _, table := range layout.sessions {
		// The table name is one of the two constants above, never anything read
		// from the database, so interpolating it cannot inject SQL.
		rows, err := db.Query(`SELECT id FROM ` + table +
			` WHERE parent_id IS NULL OR parent_id = '' ORDER BY time_created, id`)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", table, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", table, err)
		}
	}
	return out, nil
}

// openCodeSessionRow is what a session row tells us about a session: where it
// ran, what it is called, and the model it is recorded against.
type openCodeSessionRow struct {
	directory string
	title     string
	agent     string
	model     sql.NullString
	created   int64
	updated   int64
}

// openCodeReadSession finds a session in whichever session table holds it.
//
// OpenCode 2's copy is consulted first. For a migrated session the two agree,
// and where they do not, the table the current release writes is the one whose
// contents the user is looking at.
func openCodeReadSession(db *sql.DB, layout openCodeLayout, sessionID string) (openCodeSessionRow, error) {
	for _, table := range layout.sessions {
		// The table name is one of the two constants above, never anything read
		// out of the database, so interpolating it cannot inject SQL.
		row := db.QueryRow(
			`SELECT directory, title, agent, model, time_created, time_updated FROM `+table+
				` WHERE id = ?`, sessionID)
		var out openCodeSessionRow
		var title, agent sql.NullString
		err := row.Scan(&out.directory, &title, &agent, &out.model, &out.created, &out.updated)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return openCodeSessionRow{}, err
		}
		out.title = title.String
		out.agent = agent.String
		return out, nil
	}
	return openCodeSessionRow{}, fmt.Errorf("no OpenCode session %q", sessionID)
}

func (p *OpenCodeParser) parseWith(db *sql.DB, layout openCodeLayout, sessionID string, prev model.ScanState, pricer *Pricer) (model.SessionWrite, model.ScanState, error) {
	next := model.ScanState{
		Path:       "opencode:" + sessionID,
		Agent:      model.AgentOpencode,
		Cursor:     model.CursorTimeMax,
		SessionUID: sessionID,
		ModTime:    prev.ModTime,
		Offset:     prev.Offset,
		Size:       fileSize(p.DBPath),
	}

	sess, err := openCodeReadSession(db, layout, sessionID)
	if err != nil {
		return model.SessionWrite{}, next, err
	}

	b := newSessionBuilder(sessionID, model.AgentOpencode, sess.directory)
	b.title = sess.title
	if sess.title == "" {
		b.title = filepath.Base(sess.directory)
	}

	// Resume past everything already ingested. The per-message and per-part
	// cursors below are what actually guarantee nothing is missed; the session
	// row's own update time is only a bound on how much there could be.
	since := prev.Offset

	modelName := opencodeModelName(sess.model)
	msgs, err := openCodeMessages(db, layout, sessionID, since)
	if err != nil {
		return model.SessionWrite{}, next, err
	}

	// Parts are keyed by message and carry no ordering of their own, so each
	// message's parts are sorted before rendering. rowid is the tiebreak: two
	// parts created in the same millisecond otherwise come out in arbitrary
	// order, which can put a tool call after the text it produced.
	partsByMsg := map[string][]partRow{}
	if layout.hasParts {
		partsByMsg, err = openCodeParts(db, sessionID, since)
		if err != nil {
			return model.SessionWrite{}, next, err
		}
	}
	inBatch := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		inBatch[m.id] = true
	}

	maxCursor := since
	for _, m := range msgs {
		if m.updated > maxCursor {
			maxCursor = m.updated
		}
		for _, pr := range partsByMsg[m.id] {
			if pr.updated > maxCursor {
				maxCursor = pr.updated
			}
		}
		p.consumeMessage(b, m, partsByMsg[m.id], modelName, pricer)
	}

	// A part can be newer than the message it belongs to, because OpenCode
	// updates the two rows independently: a tool result lands on an assistant
	// message whose row is not itself touched. The message cursor then does not
	// bring that message into this batch, and since a part is only ever rendered
	// by walking the messages, the part was read, dropped, and — because the
	// cursor below advanced past it — never read again. The tool call was lost
	// permanently, not merely late.
	//
	// So every part whose message is not in the batch is rendered against its
	// message directly. Its message row is fetched by id rather than filtered by
	// the cursor, since the whole point is that the cursor did not select it.
	// Passing no message data means no call is produced for it: the message was
	// already ingested under the same call key on the pass that did read it, and
	// only its parts are outstanding.
	for id, list := range partsByMsg {
		if inBatch[id] {
			continue
		}
		created, ok := messageCreated(db, layout, id)
		if !ok {
			// The message row is gone. Its parts cannot be attributed to
			// anything, and re-reading them on every future pass would stall the
			// cursor for a row that will never arrive, so they are dropped and
			// the cursor is allowed past them.
			continue
		}
		for _, pr := range list {
			if pr.updated > maxCursor {
				maxCursor = pr.updated
			}
		}
		p.consumeMessage(b, openCodeMsg{id: id, created: created}, list, modelName, pricer)
	}

	next.Offset = maxCursor
	next.Size = sess.updated
	next.ModTime = time.UnixMilli(sess.updated).UTC()
	// The path is left empty: there is no transcript file to open. Filling it
	// with the synthesised key would render a link that cannot resolve.
	return b.build(""), next, nil
}

// openCodeMsg is one message row, from whichever table held it.
type openCodeMsg struct {
	id      string
	created int64
	updated int64
	// order breaks a tie between two messages created in the same millisecond.
	order int64
	// role is the message's role: "assistant" is the one that carries usage.
	// OpenCode 1 recorded it inside the JSON blob, 2 in a column of its own.
	role string
	data map[string]any
}

// The two message queries. They are spelled out rather than assembled from a
// table name so that no value read from the database ever reaches a statement.
//
// The cursor is pushed into the query rather than applied afterwards. An
// unchanged session then returns no rows at all, so its payloads are never
// decoded — which is the whole cost of a no-op pass otherwise, since every
// message and part carries a JSON blob.
const (
	openCodeMessageQueryV1 = `SELECT id, time_created, time_updated, data FROM message
		WHERE session_id = ? AND time_updated > ? ORDER BY time_created, rowid`

	openCodeMessageQueryV2 = `SELECT id, time_created, time_updated, type, seq, data
		FROM session_message
		WHERE session_id = ? AND time_updated > ? ORDER BY seq, rowid`
)

// openCodeMessages reads a session's messages out of every message table the
// database holds.
//
// Both tables are read and merged rather than one being preferred. The 2.x
// migration copies rows rather than moving them, and it does not copy all of
// them: a 1.x session can hold messages in `message` that `session_message`
// never received. Preferring the new table therefore drops real messages, and
// preferring the old one drops every session 2 created. Reading both and
// deduplicating by id loses nothing from either direction.
func openCodeMessages(db *sql.DB, layout openCodeLayout, sessionID string, since int64) ([]openCodeMsg, error) {
	var batches [][]openCodeMsg
	for _, table := range layout.messages {
		var rows *sql.Rows
		var err error
		if table == "session_message" {
			rows, err = db.Query(openCodeMessageQueryV2, sessionID, since)
		} else {
			rows, err = db.Query(openCodeMessageQueryV1, sessionID, since)
		}
		if err != nil {
			return nil, fmt.Errorf("read %s of %s: %w", table, sessionID, err)
		}
		var batch []openCodeMsg
		for rows.Next() {
			var m openCodeMsg
			var raw []byte
			var err error
			if table == "session_message" {
				var role string
				var seq int64
				if err = rows.Scan(&m.id, &m.created, &m.updated, &role, &seq, &raw); err == nil {
					m.role, m.order = role, seq
				}
			} else {
				var rowid int64
				if err = rows.Scan(&m.id, &m.created, &m.updated, &raw); err == nil {
					// rowid is 1.x's only ordering column, and it plays the
					// same part as `seq`: a monotonic per-session counter.
					m.order = rowid
				}
			}
			if err != nil {
				rows.Close()
				return nil, err
			}
			m.data = decodeObject(string(raw))
			if m.data == nil {
				continue
			}
			if m.role == "" {
				m.role = str(m.data, "role")
			}
			if m.role == "" {
				m.role = str(m.data, "type")
			}
			batch = append(batch, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s of %s: %w", table, sessionID, err)
		}
		batches = append(batches, batch)
	}
	return mergeOpenCodeMessages(batches), nil
}

// mergeOpenCodeMessages flattens the per-table reads into one ordered slice,
// keeping the first copy of each id.
//
// The earlier batch wins, which is why the tables are read most-current-first:
// where a message exists in both, the row OpenCode 2 now writes is the one kept.
func mergeOpenCodeMessages(batches [][]openCodeMsg) []openCodeMsg {
	seen := make(map[string]bool)
	var out []openCodeMsg
	for _, batch := range batches {
		for _, m := range batch {
			if seen[m.id] {
				continue
			}
			seen[m.id] = true
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].created != out[j].created {
			return out[i].created < out[j].created
		}
		return out[i].order < out[j].order
	})
	return out
}

// partRow is one row of the part table, the 1.x shape: a message's tool call,
// text or reasoning, split out as its own record.
type partRow struct {
	id      string
	updated int64
	rowid   int64
	data    map[string]any
}

// openCodeParts reads the part rows of one session.
//
// A session 2 created has none at all: 2.x keeps the whole message — tool calls
// included — in the message's own JSON. The table is still queried for every
// session, because a session migrated from 1.x does have parts, and those are
// where its tool calls are.
func openCodeParts(db *sql.DB, sessionID string, since int64) (map[string][]partRow, error) {
	byMsg := map[string][]partRow{}
	rows, err := db.Query(
		"SELECT id, message_id, time_updated, rowid, data FROM part"+
			" WHERE session_id = ? AND time_updated > ? ORDER BY rowid",
		sessionID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("read parts of %s: %w", sessionID, err)
	}
	for rows.Next() {
		var id, messageID string
		var updated, rowid int64
		var raw []byte
		if err := rows.Scan(&id, &messageID, &updated, &rowid, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if obj := decodeObject(string(raw)); obj != nil {
			byMsg[messageID] = append(byMsg[messageID],
				partRow{id: id, updated: updated, rowid: rowid, data: obj})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, list := range byMsg {
		sortParts(list)
	}
	return byMsg, nil
}

// messageCreated reads one message's creation time by id, unfiltered by any
// cursor, and reports ok=false when the row is not there.
//
// A part and its message are written independently, so a part can be newer than
// the message cursor while the message itself is older than it. The message is
// still in the database; it simply is not in this batch.
func messageCreated(db *sql.DB, layout openCodeLayout, id string) (int64, bool) {
	for _, table := range layout.messages {
		// Constant table name, as above.
		var created int64
		if err := db.QueryRow(`SELECT time_created FROM `+table+` WHERE id = ?`, id).
			Scan(&created); err == nil {
			return created, true
		}
	}
	return 0, false
}

func (p *OpenCodeParser) consumeMessage(b *sessionBuilder, m openCodeMsg, parts []partRow, sessionModel string, pricer *Pricer) {
	ts := epochMillis(m.created)
	b.observeTime(tsStr(ts))

	for _, part := range parts {
		if str(part.data, "type") != "tool" {
			continue
		}
		t := opencodeTool(part.data)
		t.Time = ts
		// Keyed by the part's own id rather than by position in this batch. A
		// positional key restarts at zero on every incremental pass, so the
		// first tool call of each increment overwrote the first tool call
		// already stored — tool counts and seconds silently lost on exactly the
		// sessions that were still running.
		t.CallKey = "part:" + part.id
		b.addTool(t)
	}

	// A session 2 created keeps its tool calls inside the message blob rather
	// than in the part table, so they are read from there — but only when the
	// message has no parts. A migrated session has both, and they are the same
	// tool calls, so preferring parts is what keeps it counted once.
	if len(parts) == 0 {
		for i, item := range asSlice(nestedAny(m.data, "content")) {
			entry := asMap(item)
			if str(entry, "type") != "tool" {
				continue
			}
			t := openCodeInlineTool(entry)
			t.Time = ts
			t.CallKey = "inline:" + m.id + ":" + openCodeInlineToolKey(entry, i)
			b.addTool(t)
		}
	}

	if m.role != "assistant" || m.data == nil {
		return
	}
	// OpenCode records zero cost for everything by design — it mostly runs local
	// and free-tier models — so the tokens on an assistant message are the only
	// accounting signal available.
	//
	// The session row also carries running token totals (session_v2's
	// tokens_input and friends). They are deliberately not used as a second
	// source: a session that recorded per-message usage would then be counted
	// twice, and the failure would be a doubled bill rather than a missing one.
	tokens := asMap(m.data["tokens"])
	if len(tokens) == 0 {
		return
	}

	input := num(tokens, "input")
	output := num(tokens, "output")
	reasoning := num(tokens, "reasoning")
	// The cache counters are nested under `cache`, so they take a lookup in the
	// sub-object rather than a flat one. Reading them as the literal keys
	// "cache.read" and "cache.write" matched nothing, so every OpenCode session
	// reported its cached tokens as zero and billed them at the input rate
	// instead of the cache rate.
	cache := asMap(tokens["cache"])
	cacheRead := num(cache, "read")
	cacheWrite := num(cache, "write")
	// Unlike Antigravity, output here excludes reasoning, so the two are summed
	// rather than carved out of one figure — and the sum is what has to be
	// billed. price() prices OutputTokens alone, which on a log that reports the
	// two separately charges for neither the reasoning nor the tokens spent
	// producing it.

	callTS := ts
	if c := parseTime(nestedAny(m.data, "time", "completed")); !c.IsZero() {
		callTS = c
	}
	llmSeconds := 0.0
	if completed := parseTime(nestedAny(m.data, "time", "completed")); !completed.IsZero() && completed.After(ts) {
		if d := completed.Sub(ts).Seconds(); d > 0 && d < 600 {
			llmSeconds = d
		}
	}

	// The message's own model first, the session's as a fallback. OpenCode 2
	// records a model switch as its own message, so the session's column names
	// the model it ended on rather than the one a given call was billed at.
	name := openCodeMessageModel(m.data)
	if name == "" {
		name = sessionModel
	}
	if name == "" {
		name = "unknown"
	}

	c := model.Call{
		Model:            name,
		Time:             callTS,
		InputTokens:      int(input),
		OutputTokens:     int(output),
		CacheReadTokens:  int(cacheRead),
		CacheWriteTokens: int(cacheWrite),
		ReasoningTokens:  int(reasoning),
		LLMSeconds:       llmSeconds,
		CallKey:          "msg:" + m.id,
	}
	priceGenerated(pricer, &c, int(output+reasoning))
	b.addCall(c)
}

// openCodeMessageModel resolves the model one message was answered by.
//
// 1.x wrote it flat as `modelID`; 2.x writes an object under `model` carrying
// the provider alongside the id. A user message, which names the model that was
// asked for rather than the one that answered, puts it inside that object as
// `modelID` — which is why that spelling is accepted too.
func openCodeMessageModel(data map[string]any) string {
	if id := str(data, "modelID"); id != "" {
		return id
	}
	m := asMap(data["model"])
	if id := str(m, "id"); id != "" {
		return id
	}
	return str(m, "modelID")
}

// openCodeInlineTool reads one tool call out of a session 2 message's content.
//
// It is the same record the 1.x part table held, spelled the way 2.x spells it:
// the name under `name` rather than `tool`, and the timing beside the state
// rather than inside it.
func openCodeInlineTool(entry map[string]any) model.ToolCall {
	name := str(entry, "name")
	state := asMap(entry["state"])
	// Input the tool could not parse is filed under a synthetic "invalid" name,
	// with the real one beside the parse error, exactly as 1.x filed it. The
	// check is for that name rather than for an empty one: "invalid" is present,
	// and reading it as the tool's name reports a tool that does not exist.
	if name == "" || name == "invalid" {
		if original := str(asMap(state["input"]), "tool"); original != "" {
			name = original
		}
	}
	if name == "" {
		name = "unknown"
	}
	times := asMap(entry["time"])
	seconds := 0.0
	started := parseTime(times["created"])
	if completed := parseTime(times["completed"]); completed.After(started) {
		// Bounded for the same reason as everything else here: a tool that was
		// never collected can report a completion hours later.
		if d := completed.Sub(started).Seconds(); d > 0 && d < 600 {
			seconds = d
		}
	}
	return model.ToolCall{
		Tool:    name,
		Seconds: seconds,
		IsError: str(state, "error") != "" || str(state, "status") == "error",
	}
}

// openCodeInlineToolKey names an inline tool call stably, so re-reading the same
// message replaces its row rather than adding a second one.
func openCodeInlineToolKey(entry map[string]any, index int) string {
	if id := str(entry, "id"); id != "" {
		return id
	}
	return strconv.Itoa(index)
}

func sortParts(list []partRow) {
	sort.SliceStable(list, func(i, j int) bool {
		si := nestedAny(list[i].data, "time", "start")
		sj := nestedAny(list[j].data, "time", "start")
		if toFloat(si) != toFloat(sj) {
			return toFloat(si) < toFloat(sj)
		}
		return list[i].rowid < list[j].rowid
	})
}

func opencodeTool(part map[string]any) model.ToolCall {
	// OpenCode files input it could not parse under a synthetic "invalid" tool
	// and keeps the real name beside the parse error, so prefer the real one.
	name := str(part, "tool")
	if name == "" || name == "invalid" {
		state := asMap(part["state"])
		if original := str(asMap(state["input"]), "tool"); original != "" {
			name = original
		}
	}
	if name == "" {
		name = "unknown"
	}
	state := asMap(part["state"])
	times := asMap(state["time"])
	seconds := span(times, "start", "end")
	errMsg := str(state, "error")
	return model.ToolCall{
		Tool:    name,
		Seconds: seconds,
		IsError: errMsg != "" || str(state, "status") == "error",
	}
}

// opencodeModelName resolves the session's model column, which holds a JSON
// blob but has been observed holding a bare string or an array too. Calling
// .get on a non-object aborted the export before a byte was written.
func opencodeModelName(col sql.NullString) string {
	if !col.Valid || col.String == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(col.String), &parsed); err != nil {
		return col.String
	}
	if obj, ok := parsed.(map[string]any); ok {
		if id, ok := obj["id"].(string); ok {
			return id
		}
		return ""
	}
	return col.String
}

func nestedAny(obj map[string]any, path ...string) any {
	var cur any = obj
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}
