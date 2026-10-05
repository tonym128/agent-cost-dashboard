package source

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"time"

	"github.com/tonym/agent-cost-dashboard/internal/model"
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

	stepStart := time.Unix(startSec, 0).UTC()
	if hasStart {
		b.observeTime(tsStr(stepStart))
	}

	switch stepType {
	case agyStepLLM1, agyStepLLM2:
		usageBlob := fieldBytes(fields, agyMetaUsage)
		if len(usageBlob) == 0 {
			return
		}
		u := ProtoVarints(usageBlob)

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
type OpenCodeParser struct {
	DBPath string
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
	return p.parseWith(db, sessionID, prev, pricer)
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
	for _, id := range ids {
		sess, next, err := p.parseWith(db, id, prev[id], pricer)
		if err != nil {
			return sessions, states, err
		}
		sessions[id] = sess
		states[id] = next
	}
	return sessions, states, nil
}

func (p *OpenCodeParser) parseWith(db *sql.DB, sessionID string, prev model.ScanState, pricer *Pricer) (model.SessionWrite, model.ScanState, error) {
	next := model.ScanState{
		Path:       "opencode:" + sessionID,
		Agent:      model.AgentOpencode,
		Cursor:     model.CursorTimeMax,
		SessionUID: sessionID,
		ModTime:    prev.ModTime,
		Offset:     prev.Offset,
		Size:       fileSize(p.DBPath),
	}

	row := db.QueryRow(
		"SELECT directory, title, agent, model, time_created, time_updated FROM session WHERE id = ?",
		sessionID)
	var directory, title, agent, modelCol sql.NullString
	var created, updated int64
	if err := row.Scan(&directory, &title, &agent, &modelCol, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return model.SessionWrite{}, next, fmt.Errorf("no OpenCode session %q", sessionID)
		}
		return model.SessionWrite{}, next, err
	}

	b := newSessionBuilder(sessionID, model.AgentOpencode, directory.String)
	b.title = title.String
	if !title.Valid || title.String == "" {
		b.title = filepath.Base(directory.String)
	}

	// Resume past everything already ingested. time_updated on the session row
	// is the cheap upper bound; the per-row cursors below are what actually
	// guarantee nothing is missed.
	since := prev.Offset

	modelName := opencodeModelName(modelCol)
	type msgRow struct {
		id      string
		created int64
		updated int64
		data    map[string]any
	}
	var msgs []msgRow
	// The cursor is pushed into the query rather than applied afterwards. An
	// unchanged session then returns no rows at all, so its payloads are never
	// decoded — which is the whole cost of a no-op pass otherwise, since every
	// message and part carries a JSON blob.
	mrows, err := db.Query(
		"SELECT id, time_created, time_updated, data FROM message"+
			" WHERE session_id = ? AND time_updated > ? ORDER BY time_created, rowid",
		sessionID, since,
	)
	if err != nil {
		return model.SessionWrite{}, next, fmt.Errorf("read messages of %s: %w", sessionID, err)
	}
	for mrows.Next() {
		var m msgRow
		var raw []byte
		if err := mrows.Scan(&m.id, &m.created, &m.updated, &raw); err != nil {
			mrows.Close()
			return model.SessionWrite{}, next, err
		}
		m.data = decodeObject(string(raw))
		if m.data == nil {
			continue
		}
		msgs = append(msgs, m)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return model.SessionWrite{}, next, err
	}

	// Parts are keyed by message and carry no ordering of their own, so each
	// message's parts are sorted before rendering. rowid is the tiebreak: two
	// parts created in the same millisecond otherwise come out in arbitrary
	// order, which can put a tool call after the text it produced.
	partsByMsg := map[string][]partRow{}
	prows, err := db.Query(
		"SELECT message_id, time_updated, rowid, data FROM part"+
			" WHERE session_id = ? AND time_updated > ? ORDER BY rowid",
		sessionID, since,
	)
	if err != nil {
		return model.SessionWrite{}, next, fmt.Errorf("read parts of %s: %w", sessionID, err)
	}
	for prows.Next() {
		var messageID string
		var updated int64
		var rowid int64
		var raw []byte
		if err := prows.Scan(&messageID, &updated, &rowid, &raw); err != nil {
			prows.Close()
			return model.SessionWrite{}, next, err
		}
		if obj := decodeObject(string(raw)); obj != nil {
			partsByMsg[messageID] = append(partsByMsg[messageID],
				partRow{updated: updated, rowid: rowid, data: obj})
		}
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return model.SessionWrite{}, next, err
	}
	for _, list := range partsByMsg {
		sortParts(list)
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
		p.consumeMessage(b, m.id, m.created, m.data, partsByMsg[m.id], modelName, pricer)
	}
	for _, list := range partsByMsg {
		for _, pr := range list {
			if pr.updated > maxCursor {
				maxCursor = pr.updated
			}
		}
	}

	next.Offset = maxCursor
	next.Size = int64(updated)
	next.ModTime = time.UnixMilli(updated).UTC()
	// The path is left empty: there is no transcript file to open. Filling it
	// with the synthesised key would render a link that cannot resolve.
	return b.build(""), next, nil
}

type partRow struct {
	updated int64
	rowid   int64
	data    map[string]any
}

func (p *OpenCodeParser) consumeMessage(b *sessionBuilder, id string, created int64, data map[string]any, parts []partRow, modelName string, pricer *Pricer) {
	ts := epochMillis(created)
	b.observeTime(tsStr(ts))

	for _, part := range parts {
		if str(part.data, "type") != "tool" {
			continue
		}
		t := opencodeTool(part.data)
		t.Time = ts
		b.addTool(t)
	}

	if str(data, "role") != "assistant" {
		return
	}
	// OpenCode records zero cost for everything by design — it mostly runs local
	// and free-tier models — so the tokens on an assistant message are the only
	// accounting signal available.
	tokens := asMap(data["tokens"])
	if len(tokens) == 0 {
		return
	}

	input := num(tokens, "input")
	output := num(tokens, "output")
	reasoning := num(tokens, "reasoning")
	cacheRead := num(tokens, "cache.read")
	cacheWrite := num(tokens, "cache.write")
	// Unlike Antigravity, output here excludes reasoning, so the two are summed
	// rather than carved out of one figure.

	callTS := ts
	if c := parseTime(nestedAny(data, "time", "completed")); !c.IsZero() {
		callTS = c
	}
	llmSeconds := 0.0
	if completed := parseTime(nestedAny(data, "time", "completed")); !completed.IsZero() && completed.After(ts) {
		if d := completed.Sub(ts).Seconds(); d > 0 && d < 600 {
			llmSeconds = d
		}
	}

	name := modelName
	if name == "" {
		name = str(data, "modelID")
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
		CallKey:          "msg:" + id,
	}
	price(pricer, &c)
	b.addCall(c)
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
