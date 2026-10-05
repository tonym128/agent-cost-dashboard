package source

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// jsonlParser is the shared shape of the four agents that write one JSON object
// per line: pi/omp, Claude Code, Codex CLI and Gemini CLI.
//
// What differs between them is the record schema, which each supplies through
// the consume hook. Everything structural — resuming, cursor management, session
// identity, pricing — is identical and lives here.
type jsonlParser struct {
	agent   string
	consume func(b *sessionBuilder, rec record, ctx *parseCtx) error
	// uidFrom lets a parser that reads the id from the log body supply it, by
	// peeking at the first records. Returns "" when the log carries none.
	uidFrom func(recs []record) string
	// projectFrom likewise supplies the working directory from the log body.
	projectFrom func(path string) string
	// resumeBaseline supplies the cumulative figure a parser differences against
	// when an incremental read starts mid-file, read from the log rather than
	// carried in the cursor. Only a parser whose records are running totals needs
	// one; see codexPriorTotals.
	resumeBaseline func(path string, from int64) map[string]any
}

// parseCtx carries what a consume hook needs beyond the record itself.
type parseCtx struct {
	pricer *Pricer
	// pendingToolCalls maps a tool call id to the request that issued it, so a
	// tool result found several lines later can be attributed to the right call.
	pendingToolCalls map[string]pendingTool
	// lastRequest is the timestamp of the most recent user message or tool
	// result, which is the lower bound for how long the next response took.
	lastRequest time.Time
}

type pendingTool struct {
	name string
	ts   time.Time
}

// Parse implements Parser.
func (p *jsonlParser) Parse(path string, prev model.ScanState, pricer *Pricer) (model.SessionWrite, model.ScanState, error) {
	from, next := resumePoint(path, prev)
	recs, consumed, err := scanLines(path, from)
	if err != nil {
		return model.SessionWrite{}, prev, fmt.Errorf("read %s: %w", path, err)
	}

	next.Path = path
	next.Agent = p.agent
	next.Offset = consumed
	next.Cursor = model.CursorBytes
	next.PrefixHash, next.PrefixLen = headFingerprint(path)
	next.ModTime = modTime(path)
	if st, err := os.Stat(path); err == nil {
		// Size is half of the "has this file changed" check the scanner runs
		// before deciding to re-read anything.
		next.Size = st.Size()
	}

	// A first pass over the head of the file supplies the session id and working
	// directory when the log body carries them. Only done on a full read: an
	// increment starts past the header, so the values must come from what is
	// already stored.
	uid := ""
	project := ""
	if from == 0 {
		if p.uidFrom != nil {
			uid = p.uidFrom(recs)
		}
		if p.projectFrom != nil {
			project = p.projectFrom(path)
		}
		if uid == "" {
			uid = stem(path)
		}
		next.SessionUID = uid
	} else {
		uid = prev.SessionUID
		if uid == "" {
			uid = stem(path)
			next.SessionUID = uid
		}
	}

	b := newSessionBuilder(uid, p.agent, project)
	// Seed the ordinal from the resume point so a fallback call key stays
	// stable across scans. A pass that reads nothing new is the normal case
	// once a log has settled, so the empty case has to be handled rather than
	// indexing into an empty slice.
	if len(recs) > 0 {
		b.ordinal = recs[0].Ordinal
	}
	ctx := &parseCtx{pricer: pricer, pendingToolCalls: map[string]pendingTool{}}

	// A parser whose records are cumulative needs the total as it stood at the
	// cursor, which a resumed read starts after. It is recovered from the log
	// itself rather than from the cursor, because the cursor's fields all have
	// another meaning already.
	if from > 0 && p.resumeBaseline != nil {
		b.prevTotalsHook = func() map[string]any { return p.resumeBaseline(path, from) }
	}

	for _, rec := range recs {
		if rec.Data == nil {
			continue
		}
		if err := p.consume(b, rec, ctx); err != nil {
			// A record that cannot be understood is skipped rather than
			// aborting: one malformed line must not cost the rest of the file.
			continue
		}
	}
	return b.build(path), next, nil
}

func (p *jsonlParser) Agent() string { return p.agent }

// SessionUID answers "which session does this file hold" without ingesting it,
// for callers that need the id before a scan (the transcript endpoint).
func (p *jsonlParser) SessionUID(path string) string {
	if p.uidFrom != nil {
		if recs, _, err := scanLines(path, 0); err == nil {
			if id := p.uidFrom(recs); id != "" {
				return id
			}
		}
	}
	return stem(path)
}

// resumePoint decides where to start reading.
//
// A resume is only valid when the file still begins with what it did before.
// Otherwise the earlier offset refers to different bytes, and appending from it
// would blend two histories together — so the caller gets a full re-read instead.
func resumePoint(path string, prev model.ScanState) (from int64, next model.ScanState) {
	next = prev
	if prev.Offset <= 0 {
		return 0, next
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, next
	}
	// A file that shrank was rotated or truncated.
	if st.Size() < prev.Offset {
		return 0, next
	}
	if prev.Cursor != "" && prev.Cursor != model.CursorBytes {
		return 0, next
	}
	if !HeadMatches(path, prev.PrefixHash, prev.PrefixLen) {
		return 0, next
	}
	return prev.Offset, next
}

func modTime(path string) time.Time {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// ---------------------------------------------------------------- pi / omp

// PiParser reads pi and omp session logs.
//
// The format is a `session` header record followed by `message` records. Usage
// lives on the assistant message, and the response time for a call is measured
// from the preceding user message or tool result — the same definition the rest
// of the dashboard uses for "how long did a response take".
func NewPiParser() Parser {
	return &jsonlParser{
		agent: model.AgentPi,
		uidFrom: func(recs []record) string {
			for _, r := range recs {
				if r.Data == nil || str(r.Data, "type") != "session" {
					continue
				}
				if id := str(r.Data, "id"); id != "" {
					return id
				}
			}
			return ""
		},
		projectFrom: func(path string) string {
			return projectFromSessionRecord(path)
		},
		consume: consumePi,
	}
}

func consumePi(b *sessionBuilder, rec record, ctx *parseCtx) error {
	d := rec.Data
	ts := parseTime(d["timestamp"])
	switch str(d, "type") {
	case "session":
		b.setProject(str(d, "cwd"))
		b.observeTime(tsStr(ts))
		return nil
	case "message":
	default:
		return nil
	}

	msg := asMap(d["message"])
	role := str(msg, "role")
	content := asSlice(msg["content"])

	// A tool result closes the previous turn and opens a new request window.
	for _, item := range content {
		it := asMap(item)
		switch str(it, "type") {
		case "toolCall", "tool_call":
			id := str(it, "id")
			if id == "" {
				id = str(it, "toolCallId")
			}
			name := str(it, "name")
			if name == "" {
				name = "tool"
			}
			ctx.pendingToolCalls[id] = pendingTool{name: name, ts: ts}
		case "toolResult", "tool_result":
			id := str(it, "toolCallId")
			if id == "" {
				id = str(it, "id")
			}
			pending, ok := ctx.pendingToolCalls[id]
			if !ok {
				pending = pendingTool{name: "tool"}
			}
			isErr := str(it, "isError") == "true" || boolOf(it["isError"])
			b.addTool(model.ToolCall{
				Tool:    pending.name,
				Time:    ts,
				Seconds: span(it, "startTime", "endTime"),
				IsError: isErr,
			})
			ctx.lastRequest = ts
		case "text":
			// User prose: opens a new request window.
			if role == "user" {
				ctx.lastRequest = ts
			}
		}
	}
	if role == "user" && !ts.IsZero() {
		ctx.lastRequest = ts
	}

	usage := asMap(msg["usage"])
	if len(usage) == 0 {
		return nil
	}
	model_ := str(msg, "model")
	if model_ == "" {
		model_ = "unknown"
	}

	// pi reports input and output; some builds add cache and reasoning splits.
	input := num(usage, "input")
	if input == 0 {
		input = num(usage, "inputTokens")
	}
	output := num(usage, "output")
	if output == 0 {
		output = num(usage, "outputTokens")
	}
	cacheRead := num(usage, "cacheRead")
	cacheRead += num(usage, "cacheReadInputTokens")
	cacheWrite := num(usage, "cacheWrite")
	reasoning := num(usage, "reasoning")

	// Reasoning, where reported, is a slice of output rather than an addition.
	if reasoning > 0 && output >= reasoning {
		output -= reasoning
	}

	llmSeconds := 0.0
	if !ts.IsZero() && !ctx.lastRequest.IsZero() {
		if delta := ts.Sub(ctx.lastRequest).Seconds(); delta > 0 && delta < 600 {
			llmSeconds = delta
		}
	}
	ctx.lastRequest = time.Time{}

	c := model.Call{
		Model:            model_,
		Time:             ts,
		InputTokens:      int(input),
		OutputTokens:     int(output),
		CacheReadTokens:  int(cacheRead),
		CacheWriteTokens: int(cacheWrite),
		ReasoningTokens:  int(reasoning),
		LLMSeconds:       llmSeconds,
	}
	if id := str(d, "id"); id != "" {
		c.CallKey = "msg:" + id
	}
	price(ctx.pricer, &c)
	b.addCall(c)
	b.observeTime(tsStr(ts))
	return nil
}

// ---------------------------------------------------------------- Claude Code

// ClaudeParser reads Claude Code logs.
//
// Usage sits under message.usage. Anthropic's input_tokens excludes the cache
// buckets, so the four components are summed rather than subtracted — which is
// the opposite convention to Gemini's, and the reason each parser is explicit
// about it. Anthropic also reports extended thinking as part of output_tokens
// rather than separately, so the thinking block is split back out here for the
// cost breakdown while the whole generated count is still billed.
func NewClaudeParser() Parser {
	return &jsonlParser{
		agent: model.AgentClaude,
		uidFrom: func(recs []record) string {
			return ""
		},
		consume: consumeClaude,
	}
}

func consumeClaude(b *sessionBuilder, rec record, ctx *parseCtx) error {
	d := rec.Data
	ts := parseTime(d["timestamp"])
	b.observeTime(tsStr(ts))
	if id := str(d, "sessionId"); id != "" && b.uid == "" {
		b.uid = id
	}
	if cwd := str(d, "cwd"); cwd != "" {
		b.setProject(cwd)
	}

	typ := str(d, "type")
	if typ == "user" && !ts.IsZero() {
		ctx.lastRequest = ts
	}

	// A user record carrying tool results closes a turn.
	if typ == "user" {
		msg := asMap(d["message"])
		for _, item := range asSlice(msg["content"]) {
			it := asMap(item)
			if str(it, "type") != "tool_result" {
				continue
			}
			b.addTool(model.ToolCall{
				Tool:    "tool",
				Time:    ts,
				IsError: boolOf(it["is_error"]),
			})
			ctx.lastRequest = ts
		}
		return nil
	}
	if typ != "assistant" {
		return nil
	}

	msg := asMap(d["message"])
	modelName := str(msg, "model")
	if modelName == "<synthetic>" || modelName == "" {
		return nil
	}
	usage := asMap(msg["usage"])
	if len(usage) == 0 {
		return nil
	}
	content := asSlice(msg["content"])

	input := num(usage, "input_tokens")
	generated := num(usage, "output_tokens")
	cacheRead := num(usage, "cache_read_input_tokens")
	// cache_creation_input_tokens is the aggregate; the per-window breakdown
	// underneath it must not also be added, or cache writes double count.
	cacheWrite := num(usage, "cache_creation_input_tokens")

	// Anthropic bills extended thinking at the output rate and folds it into
	// output_tokens rather than reporting it separately, so without this split
	// a thinking-heavy session shows one large output line item and the actual
	// driver of the spend is invisible.
	visible, reasoning := visibleOutput(generated, claudeReasoning(usage, content))

	llmSeconds := 0.0
	if !ts.IsZero() && !ctx.lastRequest.IsZero() {
		if delta := ts.Sub(ctx.lastRequest).Seconds(); delta > 0 && delta < 600 {
			llmSeconds = delta
		}
	}
	ctx.lastRequest = time.Time{}

	for _, item := range content {
		it := asMap(item)
		if str(it, "type") == "tool_use" {
			b.addTool(model.ToolCall{Tool: str(it, "name"), Time: ts})
		}
	}

	c := model.Call{
		Model:            modelName,
		Time:             ts,
		InputTokens:      int(input),
		OutputTokens:     int(visible),
		CacheReadTokens:  int(cacheRead),
		CacheWriteTokens: int(cacheWrite),
		ReasoningTokens:  int(reasoning),
		LLMSeconds:       llmSeconds,
	}
	if id := str(d, "uuid"); id != "" {
		c.CallKey = "msg:" + id
	}
	// The whole generated count is billed at the output rate, thinking included,
	// so the cost is unchanged by the split above.
	priceGenerated(ctx.pricer, &c, int(generated))
	b.addCall(c)
	return nil
}

// charsPerToken is the usual English-text ratio, used only to size a thinking
// block that arrived as text. It is an estimate and is labelled as one wherever
// it is reported.
const charsPerToken = 4

// claudeReasoning returns how many of a Claude Code call's generated tokens were
// extended thinking.
//
// Anthropic's usage block has no reasoning field — thinking is billed as output
// and appears only as a `thinking` content block — so the count has to come from
// somewhere else, in this order of preference:
//
//   - an explicit usage field, if a version ever adds one (checked under every
//     spelling seen across providers rather than assuming one);
//   - otherwise the size of the thinking text, divided by a standard
//     characters-per-token ratio.
//
// Both paths are best-effort. A record with neither yields 0, which shows as
// "no reasoning" rather than as a fabricated number, and visibleOutput clamps the
// result to the generated count that contains it.
func claudeReasoning(usage map[string]any, content []any) int64 {
	for _, key := range []string{
		"reasoning_tokens", "thinking_tokens", "output_reasoning_tokens",
	} {
		if v := num(usage, key); v > 0 {
			return v
		}
	}
	// Some builds nest the split one level down.
	for _, key := range []string{"output_tokens_details", "output_tokens_detail", "completion_tokens_details"} {
		details := asMap(usage[key])
		for _, sub := range []string{"reasoning_tokens", "thinking_tokens"} {
			if v := num(details, sub); v > 0 {
				return v
			}
		}
	}

	var chars int64
	for _, item := range content {
		it := asMap(item)
		switch str(it, "type") {
		case "thinking", "reasoning":
			chars += int64(len(str(it, "thinking"))) + int64(len(str(it, "reasoning")))
		case "redacted_thinking":
			// The body is withheld by design, so only its shape is known. It is
			// still thinking, so it counts, but the count is a floor.
			chars += int64(len(str(it, "data")))
		}
	}
	return chars / charsPerToken
}

// ---------------------------------------------------------------- Codex CLI

// CodexParser reads Codex CLI rollout files.
//
// Codex reports usage as a running total in token_count events rather than per
// call, so the deltas between consecutive totals are what a call consumed. The
// first total is a baseline, not a call; only the increments are recorded.
func NewCodexParser() Parser {
	return &jsonlParser{
		agent: model.AgentCodex,
		uidFrom: func(recs []record) string {
			for _, r := range recs {
				if r.Data == nil || str(r.Data, "type") != "session_meta" {
					continue
				}
				if id := str(asMap(r.Data["payload"]), "id"); id != "" {
					return id
				}
			}
			return ""
		},
		projectFrom: func(path string) string {
			return projectFromSessionRecord(path)
		},
		consume: consumeCodex,
		resumeBaseline: func(path string, from int64) map[string]any {
			return lastCodexTotals(path, from)
		},
	}
}

// lastCodexTotals returns the cumulative usage total in force at byte offset
// `from` — the last total_token_usage written before it.
//
// Codex reports usage as a session running total, so an increment is only
// meaningful against its predecessor. On a resumed read that predecessor was
// recorded before the cursor and is not in memory: the total lives in the
// builder, which is constructed fresh per Parse and carries nothing across
// scans. Without recovering it here, the first token_count of every incremental
// read sees an empty baseline and, absent an explicit last_token_usage, is
// discarded as if it were a baseline — but it is not one. Its predecessor was
// billed on the previous pass, so the increment is dropped and a rollout, which
// grows continuously and is polled every 30s, loses one call's usage per scan.
//
// Reading backwards is bounded by the last complete line before the offset; a
// rollout's token_count events are small and frequent, so this is a short read
// rather than a re-scan of the file.
func lastCodexTotals(path string, from int64) map[string]any {
	if from <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// The total must come from a line that ended at or before `from`, so the
	// search never crosses the cursor: a token_count written after it has not
	// been billed yet and is not the baseline for the record being read.
	nl := []byte("\n")
	const chunk = 64 << 10
	end := from
	buf := make([]byte, 0, chunk)
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		blk := make([]byte, end-start)
		if _, err := f.ReadAt(blk, start); err != nil && err != io.EOF {
			return nil
		}
		buf = append(blk, buf...)
		// Only whole lines are candidates: a partial first line is a fragment of
		// something longer, and reading a truncated JSON object would yield
		// whatever fields happened to parse.
		lines := bytes.Split(buf, nl)
		if start > 0 {
			lines = lines[1:]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			rec := decodeObject(string(line))
			if rec == nil {
				continue
			}
			if totals := codexTotalsOf(rec); totals != nil {
				return totals
			}
		}
		if start == 0 {
			return nil
		}
		end = start
	}
	return nil
}

// codexTotalsOf pulls the cumulative usage total out of one rollout record, or
// nil when the record is not a token_count event carrying one.
func codexTotalsOf(rec map[string]any) map[string]any {
	if str(rec, "type") != "event_msg" {
		return nil
	}
	payload := asMap(rec["payload"])
	if str(payload, "type") != "token_count" {
		return nil
	}
	total := asMap(asMap(payload["info"])["total_token_usage"])
	if len(total) == 0 {
		return nil
	}
	return total
}

func consumeCodex(b *sessionBuilder, rec record, ctx *parseCtx) error {
	d := rec.Data
	payload := asMap(d["payload"])
	ts := parseTime(d["timestamp"])

	switch str(d, "type") {
	case "session_meta":
		b.setProject(str(payload, "cwd"))
		b.observeTime(tsStr(ts))
		return nil
	case "turn_context":
		if m := str(payload, "model"); m != "" && b.title == "" {
			b.title = m
		}
		return nil
	case "event_msg":
	default:
		return nil
	}
	if str(payload, "type") != "token_count" {
		return nil
	}

	info := asMap(payload["info"])
	total := asMap(info["total_token_usage"])
	last := asMap(info["last_token_usage"])
	if len(total) == 0 && len(last) == 0 {
		return nil
	}

	// The baseline this event is differenced against, captured before the
	// running total is replaced below. Reading b.prevTotals from inside the
	// closure instead would read the total already stored there and difference
	// it against itself, so every increment came out as zero.
	prevTotals := b.baselineTotals()

	// Prefer the explicit per-call block; otherwise difference the running
	// total against the previous one.
	//
	// Presence, not value, decides which: Codex writes `cached_input_tokens: 0`
	// and `reasoning_output_tokens: 0` explicitly for a call that hit neither,
	// so testing the value fell through to the cumulative figure and billed a
	// whole session's cache reads to a single call. On the reviewer's shape
	// (total 40200/38000, last 200/0) that produced CacheReadTokens=38000,
	// InputTokens=0 and ReasoningTokens=60 — 38,000 cache tokens on a call that
	// read no cache, with the real 200 input tokens clamped away.
	//
	// A decrease means the counter was reset rather than that a call was
	// refunded. The increment is unknown, not negative, so this event
	// contributes nothing: reporting the new total in full would double count
	// everything before the reset, and reporting a negative figure would be
	// worse than reporting none.
	delta := func(key string) int64 {
		if _, present := last[key]; present {
			return num(last, key)
		}
		d := num(total, key) - num(prevTotals, key)
		if d < 0 {
			return 0
		}
		return d
	}
	if len(prevTotals) == 0 {
		if len(last) == 0 {
			b.prevTotals = total
			return nil
		}
	}
	if len(total) > 0 {
		b.prevTotals = total
	}

	rawInput := delta("input_tokens")
	cacheRead := delta("cached_input_tokens")
	// Codex's input includes the cached part; store the net so input and cache
	// read are not added twice.
	input := rawInput - cacheRead
	if input < 0 {
		input = 0
	}
	output := delta("output_tokens")
	reasoning := delta("reasoning_output_tokens")
	if reasoning > output {
		reasoning = output
	}

	modelName := str(payload, "model")
	if modelName == "" {
		modelName = b.title
	}
	if modelName == "" {
		modelName = "unknown"
	}

	c := model.Call{
		Model:           modelName,
		Time:            ts,
		InputTokens:     int(input),
		OutputTokens:    int(output),
		CacheReadTokens: int(cacheRead),
		ReasoningTokens: int(reasoning),
	}
	price(ctx.pricer, &c)
	b.addCall(c)
	b.observeTime(tsStr(ts))
	return nil
}

// ---------------------------------------------------------------- Gemini CLI

// GeminiParser reads Gemini CLI logs.
//
// Two things differ from every other agent here. Its "input" figure is inclusive
// of cached tokens, so the input bucket is the net after subtracting cache reads
// rather than a separate figure to add. And its session header and bookkeeping
// records share the file with the turns, so records without a token block must
// be skipped explicitly: treating them as turns aborted the rest of the file and
// silently discarded whole sessions.
func NewGeminiParser() Parser {
	return &jsonlParser{
		agent: model.AgentGemini,
		// Gemini records no working directory in the session log itself, so the
		// project is recovered from the history directory it mirrors the session
		// layout into. Without this every Gemini session files itself under an
		// empty project and the per-project rollups silently lose that agent.
		projectFrom: geminiProjectForPath,
		uidFrom: func(recs []record) string {
			for _, r := range recs {
				if r.Data == nil {
					continue
				}
				if id := str(r.Data, "sessionId"); id != "" {
					return id
				}
			}
			return ""
		},
		consume: consumeGemini,
	}
}

// projectFromSessionRecord reads the working directory out of the header record
// of the log at path.
//
// Header-derived rather than layout-derived: pi and codex both record a cwd
// explicitly, and reading it costs a few lines of JSON already being parsed.
func projectFromSessionRecord(path string) string {
	recs, _, err := scanLines(path, 0)
	if err != nil {
		return ""
	}
	for _, r := range recs {
		if r.Data == nil {
			continue
		}
		switch str(r.Data, "type") {
		case "session":
			if cwd := str(r.Data, "cwd"); cwd != "" {
				return cwd
			}
		case "session_meta":
			if cwd := str(asMap(r.Data["payload"]), "cwd"); cwd != "" {
				return cwd
			}
		}
	}
	return ""
}

// geminiProjectForPath recovers the working directory for one Gemini session.
//
// The log lives at <root>/<project>/chats/session-*.jsonl and the history
// directory mirrors that as <home>/.gemini/history/<project>/.project_root.
func geminiProjectForPath(path string) string {
	// .../<project>/chats/<file>.jsonl
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "chats" {
		return ""
	}
	project := filepath.Base(filepath.Dir(dir))
	if project == "" || project == "." || project == string(filepath.Separator) {
		return ""
	}
	root := filepath.Join(os.Getenv("HOME"), ".gemini", "history", project, ".project_root")
	data, err := os.ReadFile(root)
	if err != nil {
		// No history entry: the project name is the best available label, and is
		// better than grouping every Gemini session under "".
		return project
	}
	if root := strings.TrimSpace(string(data)); root != "" {
		return root
	}
	return project
}

func consumeGemini(b *sessionBuilder, rec record, ctx *parseCtx) error {
	d := rec.Data
	// Gemini uses startTime on the header and timestamp elsewhere.
	ts := parseTime(d["timestamp"])
	if ts.IsZero() {
		ts = parseTime(d["startTime"])
	}
	typ := str(d, "type")

	if typ == "user" {
		b.observeTime(tsStr(ts))
		if !ts.IsZero() {
			ctx.lastRequest = ts
		}
		return nil
	}
	if typ != "gemini" {
		return nil
	}

	tokens, _ := d["tokens"].(map[string]any)
	if len(tokens) == 0 {
		// Header or bookkeeping record, not a turn.
		return nil
	}

	cacheRead := num(tokens, "cached")
	rawInput := num(tokens, "input")
	input := rawInput - cacheRead
	if input < 0 {
		input = 0
	}
	output := num(tokens, "output")
	cacheWrite := num(tokens, "cacheWrite")
	reasoning := num(tokens, "thoughtsTokenCount")
	if reasoning > output {
		reasoning = output
	}
	output -= reasoning

	llmSeconds := 0.0
	if !ts.IsZero() && !ctx.lastRequest.IsZero() {
		if delta := ts.Sub(ctx.lastRequest).Seconds(); delta > 0 && delta < 300 {
			llmSeconds = delta
		}
	}
	ctx.lastRequest = time.Time{}

	for _, tc := range asSlice(d["toolCalls"]) {
		t := asMap(tc)
		name := str(t, "name")
		if name == "" {
			name = "tool"
		}
		b.addTool(model.ToolCall{Tool: name, Time: ts})
	}

	modelName := str(d, "model")
	if modelName == "" {
		modelName = "unknown"
	}
	c := model.Call{
		Model:            modelName,
		Time:             ts,
		InputTokens:      int(input),
		OutputTokens:     int(output),
		CacheReadTokens:  int(cacheRead),
		CacheWriteTokens: int(cacheWrite),
		ReasoningTokens:  int(reasoning),
		LLMSeconds:       llmSeconds,
	}
	if id := str(d, "id"); id != "" {
		c.CallKey = "msg:" + id
	}
	price(ctx.pricer, &c)
	b.addCall(c)
	b.observeTime(tsStr(ts))
	return nil
}

// ---------------------------------------------------------------- helpers

func tsStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// span reads a duration from a pair of timestamp fields, in either direction.
func span(obj map[string]any, startKey, endKey string) float64 {
	start := parseTime(obj[startKey])
	end := parseTime(obj[endKey])
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return 0
	}
	return end.Sub(start).Seconds()
}
