package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

func testPricer(t *testing.T) *Pricer {
	t.Helper()
	p, err := NewPricer("../../models.json", "manual_pricing.json")
	if err != nil {
		t.Fatalf("NewPricer: %v", err)
	}
	return p
}

func TestNormalizeModel(t *testing.T) {
	// The three shapes the same model arrives in must collapse to one key, or a
	// dated snapshot of a known model prices at zero.
	for _, in := range []string{
		"claude-opus-4-8",
		"anthropic/claude-opus-4.8",
		"claude-opus-4-8-20260528",
		"CLAUDE-OPUS-4-8",
	} {
		if got := NormalizeModel(in); got != "claude-opus-4-8" {
			t.Errorf("NormalizeModel(%q) = %q, want claude-opus-4-8", in, got)
		}
	}
}

func TestHyphenateStripsTierSuffix(t *testing.T) {
	// Antigravity identifies models by display name plus a tier, so every tier of
	// one model has to reduce to the same key.
	a := Hyphenate("Gemini 3.1 Pro (High)")
	b := Hyphenate("Gemini 3.1 Pro (Low)")
	if a != b {
		t.Errorf("tiers disagree: %q vs %q", a, b)
	}
	if a != "gemini-3.1-pro" {
		t.Errorf("Hyphenate = %q, want gemini-3.1-pro", a)
	}
}

func TestVisibleOutputIsNeverNegative(t *testing.T) {
	// Reasoning exceeding the generated count is a bad blob; letting it through
	// would put a negative token count into every downstream total.
	visible, reasoning := visibleOutput(100, 150)
	if visible < 0 {
		t.Errorf("visible output = %d, want >= 0", visible)
	}
	if reasoning != 100 {
		t.Errorf("reasoning = %d, want clamped to 100", reasoning)
	}
	visible, reasoning = visibleOutput(100, 30)
	if visible != 70 || reasoning != 30 {
		t.Errorf("visibleOutput(100,30) = (%d,%d), want (70,30)", visible, reasoning)
	}
}

func TestPricerPrefersLiveCatalogueThenFallback(t *testing.T) {
	p := testPricer(t)
	// gpt-oss is in the catalogue.
	if _, ok := p.Resolve("gpt-oss-120b"); !ok {
		t.Error("expected gpt-oss-120b to resolve")
	}
	// An unknown name must report unpriced rather than a confident zero.
	cost, priced := p.Cost("totally-made-up-model", 1_000_000, 0, 0, 0)
	if priced {
		t.Error("unknown model reported as priced")
	}
	if cost != 0 {
		t.Errorf("unknown model cost = %v, want 0", cost)
	}
}

func TestPricerCostArithmetic(t *testing.T) {
	p := testPricer(t)
	// A million input tokens at the published Gemini 2.5 Pro rate.
	cost, ok := p.Cost("gemini-2.5-pro", 1_000_000, 0, 0, 0)
	if !ok {
		// Not a skip. models.json is a committed fixture, so a model the tests
		// price going missing is a broken fixture rather than an environment to
		// skip for: deleting the model from the dump turned this into a
		// skip-then-pass, which reported success for a test that checked nothing.
		t.Fatal("gemini-2.5-pro is not priced. models.json is committed, so a model " +
			"the tests depend on going missing is a broken fixture, not an " +
			"environment to skip for")
	}
	if cost < 1.20 || cost > 1.30 {
		t.Errorf("1M input tokens of gemini-2.5-pro cost %v, want about 1.25", cost)
	}
}

func TestPricerCachesResolution(t *testing.T) {
	p := testPricer(t)
	p.Resolve("gemini-2.5-flash")
	before := p.cacheLen()
	p.Resolve("gemini-2.5-flash")
	p.Resolve("gemini-2.5-flash")
	if p.cacheLen() != before {
		t.Errorf("resolution is not memoised: cache grew from %d", before)
	}
}

// liveModelIDs returns every model id in the committed price dump, which is the
// set of models the dashboard will ever be asked about.
func liveModelIDs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../models.json")
	if err != nil {
		t.Fatalf("read models.json: %v", err)
	}
	var doc openRouterDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse models.json: %v", err)
	}
	ids := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		t.Fatal("models.json lists no models")
	}
	return ids
}

// TestFallbackTableNeverReportsAConfidentZero is the property the embedded table
// actually exists to provide.
//
// It used to be tested as "most fallback patterns must not be shadowed by the
// live catalogue", which measured reachability in a world where models.json
// always loads — precisely the world the table does not exist for. Shadowing is
// the healthy state: it means the dump and the fallback agree. Those entries are
// the entire reason a `go install`ed binary can still price a Claude session.
//
// So the question asked here is what happens when the dump is *absent*: every
// model in the catalogue must either get a real rate or be explicitly reported
// unpriced. The one failure worth catching is a call that claims to be priced
// while costing nothing, because that is a confident wrong number and it defeats
// the unpriced safety net entirely.
func TestFallbackTableNeverReportsAConfidentZero(t *testing.T) {
	fallback, err := NewFallbackPricer()
	if err != nil {
		t.Fatalf("NewFallbackPricer: %v", err)
	}
	// The dump itself, only to tell a genuinely free model from a mis-priced one.
	live := testPricer(t)
	ids := liveModelIDs(t)

	covered, unpriced := 0, 0
	var silentZero []string
	for _, id := range ids {
		cost, priced := fallback.Cost(id, 1_000_000, 1_000_000, 0, 0)
		if !priced {
			unpriced++
			continue
		}
		covered++
		if cost > 0 {
			continue
		}
		// Zero is only honest when the dump agrees the model is free; otherwise
		// the fallback resolved it to nothing and would report $0.00 as priced.
		if rates, ok := live.Resolve(id); ok && rates.Input+rates.Output > 0 {
			silentZero = append(silentZero, id)
		}
	}

	t.Logf("with no price dump, the embedded fallback prices %d of %d models "+
		"and reports %d as unpriced (never as $0.00)", covered, len(ids), unpriced)
	if len(silentZero) > 0 {
		sort.Strings(silentZero)
		t.Errorf("%d of %d models resolve to a confident $0.00 with no price dump: %s",
			len(silentZero), len(ids), strings.Join(silentZero, ", "))
	}
	if covered == 0 {
		t.Fatal("the embedded fallback table prices nothing at all")
	}
	// The models that matter most, by spend, must survive losing the dump. These
	// were the entries the old test wanted to delete.
	for _, id := range []string{"claude-opus-4-1", "claude-opus-4-8", "gemini-2.5-pro"} {
		if _, ok := fallback.Resolve(id); !ok {
			t.Errorf("%s is unpriced without the dump", id)
		}
	}
}

// TestFallbackShadowingIsInformationalOnly reports, without asserting on it, how
// much of the fallback table the live catalogue would shadow.
//
// This is a diagnostic for whoever refreshes prices, not a health check. A high
// count means the two sources agree; a low count means the dump has drifted and
// the table is doing real work. Neither is a reason to delete a row: every
// shadowed entry is the price that model gets when models.json is missing.
func TestFallbackShadowingIsInformationalOnly(t *testing.T) {
	p := testPricer(t)
	reach := p.FallbackReachability()
	if len(reach) == 0 {
		t.Fatal("fallback table is empty")
	}
	shadowed := 0
	for _, reachable := range reach {
		if !reachable {
			shadowed++
		}
	}
	t.Logf("informational: %d of %d fallback patterns are shadowed by the live "+
		"catalogue — those rows are the no-dump safety net, not dead entries",
		shadowed, len(reach))
}

// ---------------------------------------------------------------- protobuf

func TestProtoDecodeRejectsTruncatedVarint(t *testing.T) {
	// A partial varint used to decode to a small plausible integer, which then
	// read as a real token count.
	if got := ProtoDecode([]byte{0x05}); len(got) != 0 {
		t.Errorf("truncated varint produced %v, want nothing", got)
	}
}

func TestProtoDecodeRejectsLengthOverrun(t *testing.T) {
	// Declares a 1000-byte payload but supplies two bytes.
	blob := append([]byte{0x12, 0xe8, 0x07}, 'A', 'B')
	if got := ProtoDecode(blob); len(got) != 0 {
		t.Errorf("length overrun produced %v, want nothing", got)
	}
}

func TestProtoDecodeSkipsGroupsWithoutLosingTheRest(t *testing.T) {
	// field 1 = 42, then a group, then field 2 = 99. Returning at the group
	// would lose the trailing field.
	blob := []byte{
		0x08, 42,
		0x1b, // field 3, wire type 3 (start group)
		0x08, 7,
		0x1c, // field 3, wire type 4 (end group)
		0x10, 99,
	}
	fields := ProtoFields(blob)
	if got := fields[1].Varint; got != 42 {
		t.Errorf("field 1 = %d, want 42", got)
	}
	if got := fields[2].Varint; got != 99 {
		t.Errorf("field after group = %d, want 99 (group truncated the message)", got)
	}
}

func TestProtoDecodeNegativeInt64StaysNegative(t *testing.T) {
	// -1 as a ten-byte varint decodes to 1.8e19 without sign reinterpretation,
	// which passes any "clamp negatives" guard as an enormous count.
	blob := []byte{
		0x08,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01,
	}
	fields := ProtoDecode(blob)
	if len(fields) != 1 {
		t.Fatalf("expected one field, got %d", len(fields))
	}
	if fields[0].Varint != -1 {
		t.Errorf("field = %d, want -1", fields[0].Varint)
	}
}

func TestProtoFieldAgreesWithFullDecode(t *testing.T) {
	inner := []byte{0x08, 0x2a, 0x12, 0x03, 'a', 'b', 'c'}
	blob := []byte{0x0a, 0x05, 0x08, 0x01, 0x12, 0x02, 0x09, 0x09}
	blob = append(blob, 0x0a, byte(len(inner)))
	blob = append(blob, inner...)
	blob = append(blob, 0x12, 0x04, 'x', 'x', 'x', 'x')

	for _, num := range []int32{1, 2, 3, 9} {
		var want []byte
		for _, f := range ProtoDecode(blob) {
			if f.Num == num && f.Wire == 2 {
				want = f.Bytes
			}
		}
		got, ok := ProtoField(blob, num, 2)
		if ok != (want != nil) {
			t.Errorf("field %d: found=%v but full decode says %v", num, ok, want != nil)
			continue
		}
		if ok && string(got.Bytes) != string(want) {
			t.Errorf("field %d: targeted %q, full decode %q", num, got.Bytes, want)
		}
	}
}

// ---------------------------------------------------------------- resume

func TestScanLinesResumesAndKeepsOrdinals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	body := `{"n":1}` + "\n" + `{"n":2}` + "\n" + `{"n":3}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	recs, consumed, err := scanLines(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || consumed != int64(len(body)) {
		t.Fatalf("full read: %d records, consumed %d of %d", len(recs), consumed, len(body))
	}

	// Append two more lines and resume.
	more := `{"n":4}` + "\n" + `{"n":5}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(more)
	f.Close()

	recs2, consumed2, err := scanLines(path, consumed)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs2) != 2 {
		t.Fatalf("incremental read: %d records, want 2", len(recs2))
	}
	// Ordinals must continue from where the file left off, or a fallback call
	// key derived from them would shift and duplicate rows.
	if recs2[0].Ordinal != 3 {
		t.Errorf("first new record ordinal = %d, want 3", recs2[0].Ordinal)
	}
	if consumed2 != int64(len(body)+len(more)) {
		t.Errorf("consumed %d, want %d", consumed2, len(body)+len(more))
	}
}

func TestScanLinesIgnoresAPartialFinalLine(t *testing.T) {
	// A log an agent is still writing ends mid-record. Consuming it would either
	// drop the record or store a fragment.
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	body := `{"n":1}` + "\n" + `{"n":2}` + "\n" + `{"n":3,"half`
	os.WriteFile(path, []byte(body), 0o600)

	recs, consumed, err := scanLines(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("read %d complete records, want 2", len(recs))
	}
	if consumed != int64(len(`{"n":1}`+"\n"+`{"n":2}`+"\n")) {
		t.Errorf("consumed %d, want the offset past the last complete line", consumed)
	}

	// Completing the line makes it visible on the next pass.
	complete := `{"n":1}` + "\n" + `{"n":2}` + "\n" + `{"n":3,"half":true}` + "\n"
	os.WriteFile(path, []byte(complete), 0o600)
	recs2, _, err := scanLines(path, consumed)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs2) != 1 {
		t.Errorf("after completion read %d records, want 1", len(recs2))
	}
}

func TestResumePointRejectsARewrittenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	head := `{"n":1}` + "\n"
	tail := `{"n":2}` + "\n"
	os.WriteFile(path, []byte(head+tail), 0o600)

	hash, length := headFingerprint(path)
	state := model.ScanState{
		Offset:     int64(len(head)),
		PrefixHash: hash,
		PrefixLen:  length,
		Cursor:     model.CursorBytes,
	}
	if from, _ := resumePoint(path, state); from != state.Offset {
		t.Errorf("unchanged file resumed at %d, want %d", from, state.Offset)
	}

	// Same length, different content: a rewrite. Resuming would blend histories.
	rewritten := `{"n":9}` + "\n" + `{"n":2}` + "\n"
	os.WriteFile(path, []byte(rewritten), 0o600)
	if from, _ := resumePoint(path, state); from != 0 {
		t.Errorf("rewritten file resumed at %d, want a full re-read", from)
	}
}

func TestResumePointRejectsAShrunkenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	body := `{"n":1}` + "\n" + `{"n":2}` + "\n"
	os.WriteFile(path, []byte(body), 0o600)
	hash, length := headFingerprint(path)
	state := model.ScanState{
		Offset:     int64(len(body)),
		PrefixHash: hash,
		PrefixLen:  length,
		Cursor:     model.CursorBytes,
	}
	// Truncated by rotation.
	os.WriteFile(path, []byte(`{"n":1}`+"\n"), 0o600)
	if from, _ := resumePoint(path, state); from != 0 {
		t.Errorf("shrunken file resumed at %d, want a full re-read", from)
	}
}

func TestStableKeyIsStableAcrossCalls(t *testing.T) {
	// The fallback session id must repeat for the same path, or a log with no id
	// of its own duplicates on every scan.
	a := StableKey("/tmp/x/session.jsonl", model.AgentPi)
	b := StableKey("/tmp/x/session.jsonl", model.AgentPi)
	if a != b {
		t.Errorf("unstable key: %q vs %q", a, b)
	}
	if c := StableKey("/tmp/x/session.jsonl", model.AgentClaude); c == a {
		t.Error("different agents produced the same key")
	}
}

// TestHeadFingerprintSurvivesAnAppend guards the subtlety that makes incremental
// scanning work on small logs: if the fingerprint covered the whole file, any
// append would change it and every growing log would look rewritten.
func TestHeadFingerprintSurvivesAnAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	os.WriteFile(path, []byte(`{"n":1}`+"\n"), 0o600)

	hash, length := headFingerprint(path)
	if length == 0 {
		t.Fatal("empty fingerprint length for a non-empty file")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"n":2}` + "\n")
	f.Close()

	if !HeadMatches(path, hash, length) {
		t.Error("appending to a file changed the fingerprint of its head")
	}
	if length != int64(len(`{"n":1}`+"\n")) {
		t.Errorf("fingerprint covered %d bytes, want the whole %d-byte file",
			length, len(`{"n":1}`+"\n"))
	}
}

func TestHeadFingerprintRejectsAChangedHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	os.WriteFile(path, []byte(`{"n":1}`+"\n"), 0o600)
	hash, length := headFingerprint(path)
	os.WriteFile(path, []byte(`{"n":9}`+"\n"+`{"n":2}`+"\n"), 0o600)
	if HeadMatches(path, hash, length) {
		t.Error("a rewritten head still matched")
	}
}

// ---------------------------------------------------------------- Claude

// claudeAssistant writes a one-record Claude Code transcript — one JSON object
// on one line, as the real log has it — and returns the single call it yields.
func claudeCall(t *testing.T, msg string) model.Call {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	line := `{"type":"assistant","sessionId":"s1","cwd":"/home/u/p","uuid":"u1",` +
		`"timestamp":"2026-05-01T10:00:00Z","message":` + msg + "}\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	pricer, err := NewPricer("../../models.json", "manual_pricing.json")
	if err != nil {
		t.Fatal(err)
	}
	sw, _, err := NewClaudeParser().Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	return sw.Calls[0]
}

// thinkingBody is a realistic extended-thinking block: a few hundred characters
// of the deliberation Claude Code actually writes.
var thinkingBody = "The user wants the failing test replaced rather than the " +
	"fixture adjusted, because the fixture is the safety net. Check whether the " +
	"pricer can be constructed with no dump at all before changing anything " +
	"else, since a test that cannot build its subject proves nothing."

func TestClaudeThinkingBlockIsItemisedAsReasoning(t *testing.T) {
	// Anthropic bills thinking at the output rate and does not report it
	// separately, so without this the tokens land invisibly inside output and a
	// $14 session cannot be explained.
	call := claudeCall(t, `{"model":"claude-opus-4-5",`+
		`"usage":{"input_tokens":120,"output_tokens":400,`+
		`"cache_read_input_tokens":8000,"cache_creation_input_tokens":2000},`+
		`"content":[{"type":"thinking","thinking":`+strconv.Quote(thinkingBody)+`},`+
		`{"type":"text","text":"Understood."}]}`)

	if call.ReasoningTokens <= 0 {
		t.Fatalf("ReasoningTokens = %d, want > 0 for a thinking block", call.ReasoningTokens)
	}
	// The split must be a partition of the generated count, never more than it.
	checkGenerated(t, call, 400)
	if !call.Priced {
		t.Error("claude-opus-4-5 was not priced")
	}
	// Thinking is billed as output, so the cost must be the whole generated
	// count at the output rate — not just the visible remainder.
	rates, _ := testPricer(t).Resolve("claude-opus-4-5")
	want := 120/1e6*rates.Input + 400/1e6*rates.Output +
		8000/1e6*rates.CacheRead + 2000/1e6*rates.CacheWrite
	if diff := call.CostUSD - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("cost $%.6f, want $%.6f (whole generated count billed)", call.CostUSD, want)
	}
}

func TestClaudeWithoutThinkingHasNoReasoning(t *testing.T) {
	// The common case, and the one that must not regress into a guess: no
	// thinking block and no reasoning field means zero, not an estimate.
	call := claudeCall(t, `{"model":"claude-opus-4-5",`+
		`"usage":{"input_tokens":120,"output_tokens":400,`+
		`"cache_read_input_tokens":8000,"cache_creation_input_tokens":2000},`+
		`"content":[{"type":"text","text":"Done."},`+
		`{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}`)

	if call.ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d, want 0 for a message with no thinking block",
			call.ReasoningTokens)
	}
	if call.OutputTokens != 400 {
		t.Errorf("OutputTokens = %d, want the full 400", call.OutputTokens)
	}
	checkGenerated(t, call, 400)
}

func TestClaudeReasoningNeverExceedsGenerated(t *testing.T) {
	// A blob whose thinking dwarfs the reported output is a bad record; letting
	// the larger number through would put a token count into the dashboard that
	// is bigger than the total it is part of.
	call := claudeCall(t, `{"model":"claude-opus-4-5",`+
		`"usage":{"input_tokens":10,"output_tokens":20,"reasoning_tokens":9000},`+
		`"content":[{"type":"text","text":"ok"}]}`)

	if call.ReasoningTokens != 20 || call.OutputTokens != 0 {
		t.Errorf("reasoning %d / output %d, want clamped to (20, 0)",
			call.ReasoningTokens, call.OutputTokens)
	}
	// The clamp has to preserve the partition, not just bound one side of it: a
	// parser that clamped reasoning to the generated count while leaving output
	// alone would report the same two figures above and bill 40 generated tokens
	// for a 20-token call.
	checkGenerated(t, call, 20)
}

func TestGeminiProjectIsRecoveredFromTheHistoryDirectory(t *testing.T) {
	// Gemini records no working directory in the session log, so a parser that
	// does not consult the history directory files every Gemini session under an
	// empty project.
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := "myproject"
	histDir := filepath.Join(home, ".gemini", "history", project)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := "/home/tonym/Projects/example"
	if err := os.WriteFile(filepath.Join(histDir, ".project_root"), []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(home, ".gemini", "tmp", project, "chats", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(`{"type":"gemini","model":"m","tokens":{"input":1,"output":1}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pricer, err := NewPricer("", "")
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := NewGeminiParser().Parse(logPath, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != want {
		t.Errorf("project = %q, want %q", sess.Project, want)
	}
}

func TestGeminiFallsBackToTheProjectDirectoryName(t *testing.T) {
	// No history entry for this project: the directory name is still a better
	// label than an empty string, which would merge every such session together.
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := filepath.Join(home, ".gemini", "tmp", "no-history-here", "chats", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(`{"type":"gemini","model":"m","tokens":{"input":1,"output":1}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pricer, _ := NewPricer("", "")
	sess, _, err := NewGeminiParser().Parse(logPath, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != "no-history-here" {
		t.Errorf("project = %q, want the directory name", sess.Project)
	}
}
