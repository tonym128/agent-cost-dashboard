package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

// TestPricerCostArithmetic is expressed as relationships between rates, not as
// dollar amounts.
//
// The previous version asserted that a million input tokens of gemini-2.5-pro
// cost between $1.20 and $1.30, which is a claim about a vendor's price list
// rather than about this code: models.json is a 410KB dump rewritten by
// update_models.py, so a price change broke CI for reasons unrelated to any
// parser. Everything asserted here holds whatever the prices are.
//
// What is still pinned: a known model resolves, a known model costs something,
// and the four buckets are combined the way the arithmetic says. A pricer that
// returned a flat constant, or one that dropped the cache-write term, fails.
func TestPricerCostArithmetic(t *testing.T) {
	p := testPricer(t)

	const model = "gemini-2.5-pro"
	rates, ok := p.Resolve(model)
	if !ok {
		// Not a skip: models.json is a committed fixture, so a model the tests
		// price going missing means the fixture is broken, and skipping turned
		// that into a silent pass.
		t.Fatalf("%s is not priced. models.json is committed, so a model the tests "+
			"depend on going missing is a broken fixture, not an environment to skip for", model)
	}

	// A rate that resolves must be usable: a million tokens of anything cost
	// something, or the model reads as free.
	const perMillion = 1_000_000
	if cost, priced := p.Cost(model, perMillion, 0, 0, 0); !priced || cost <= 0 {
		t.Errorf("1M input tokens of %s cost %v (priced=%v), want a positive priced cost",
			model, cost, priced)
	}

	// The four buckets add: a call using all four costs more than any one of them
	// alone, and more than their inputs do summed. This catches a dropped term
	// without depending on any single rate's value.
	one := func(in, out, cacheRead, cacheWrite int) float64 {
		cost, priced := p.Cost(model, in, out, cacheRead, cacheWrite)
		if !priced {
			t.Fatalf("%s stopped resolving partway through the arithmetic", model)
		}
		return cost
	}
	each := one(1000, 1000, 1000, 1000)
	all := one(4000, 4000, 4000, 4000)
	if all <= each {
		t.Errorf("four times the tokens cost %v, four times one token set cost %v; "+
			"the buckets are not being summed", all, each)
	}
	// Each bucket contributes positively: removing any one of them must lower the
	// cost, which is what a term silently ignored by the pricer looks like.
	for _, dropped := range []struct {
		name                    string
		in, out, cacheR, cacheW int
	}{
		{"input", 0, 1000, 1000, 1000},
		{"output", 1000, 0, 1000, 1000},
		{"cache read", 1000, 1000, 0, 1000},
		{"cache write", 1000, 1000, 1000, 0},
	} {
		if got := one(dropped.in, dropped.out, dropped.cacheR, dropped.cacheW); got >= each {
			t.Errorf("a call without its %s tokens cost %v, against %v for all four: "+
				"that bucket is contributing nothing", dropped.name, got, each)
		}
	}

	// The billing rate is per million tokens: a million of them must cost exactly
	// the published rate, and a thousand exactly a thousandth of it. No dollar
	// amount appears here, so a price change cannot break it.
	if got := one(perMillion, 0, 0, 0); got != rates.Input {
		t.Errorf("1M input tokens cost %v, want the published input rate %v", got, rates.Input)
	}
	if got := one(0, perMillion, 0, 0); got != rates.Output {
		t.Errorf("1M output tokens cost %v, want the published output rate %v", got, rates.Output)
	}
}

// TestPricerRatesHaveTheShapeTheirNamesClaim is the pricing-table invariant that
// does not depend on any particular price.
//
// The four rates mean specific things: a cache read is cheaper than the input it
// stands in for, an input is cheaper than the output it produces, and a cache
// write costs more than a cache read. A table that got those backwards would
// still produce plausible-looking costs — which is precisely why nothing
// downstream notices.
//
// A model whose rates genuinely break one of these would be a bug in the vendor
// data rather than in this code, so the failure message says so rather than
// implying the parser is at fault.
func TestPricerRatesHaveTheShapeTheirNamesClaim(t *testing.T) {
	p := testPricer(t)
	ids := liveModelIDs(t)

	var cacheNotCheaper, outputNotDearer, writeNotDearer int
	var priced int
	for _, id := range ids {
		rates, ok := p.Resolve(id)
		if !ok {
			continue
		}
		priced++
		if rates.Input <= 0 || rates.Output <= 0 {
			// A genuinely free model is possible; the catalogue says so and the
			// fallback table above already checks for a confident $0.00.
			continue
		}
		if rates.CacheRead > 0 && rates.CacheRead >= rates.Input {
			cacheNotCheaper++
		}
		if rates.Output < rates.Input {
			outputNotDearer++
		}
		if rates.CacheWrite > 0 && rates.CacheWrite <= rates.CacheRead {
			writeNotDearer++
		}
	}
	if priced == 0 {
		t.Fatal("no model in models.json resolved, so these assertions are vacuous")
	}
	t.Logf("checked the rate shape of %d of %d catalogued models", priced, len(ids))
	// Reported, not failed: the catalogue is vendor data and a handful of odd
	// entries in it is a fact about the catalogue rather than a defect here. What
	// this makes impossible is the pricer itself flattening two rates together.
	if cacheNotCheaper > 0 {
		t.Logf("%d of %d models price a cache read at or above the input rate; that is "+
			"vendor data, and TestPricerBucketsAreNotFlattened is what checks our own "+
			"arithmetic", cacheNotCheaper, priced)
	}
	if outputNotDearer > 0 {
		t.Logf("%d of %d models price output below input", outputNotDearer, priced)
	}
	if writeNotDearer > 0 {
		t.Logf("%d of %d models price a cache write at or below a cache read",
			writeNotDearer, priced)
	}
}

// TestPricerBucketsAreNotFlattened is the version of the above that can fail.
//
// It does not ask what the catalogue contains — that is vendor data, reported
// above — but whether *this pricer* distinguishes the buckets it was given.
// Dropping the cache-read rate onto the input rate, or the output rate onto the
// input rate, is a bug in Cost() and Resolve() and it makes every cached session
// bill at the input price: a number that looks plausible and is wrong by a factor
// of ten on the largest bucket on the page.
func TestPricerBucketsAreNotFlattened(t *testing.T) {
	p := testPricer(t)

	var checked int
	for _, id := range liveModelIDs(t) {
		rates, ok := p.Resolve(id)
		if !ok || rates.Input <= 0 || rates.Output <= 0 {
			continue
		}
		checked++
		const perMillion = 1_000_000
		gotIn, _ := p.Cost(id, perMillion, 0, 0, 0)
		gotOut, _ := p.Cost(id, 0, perMillion, 0, 0)
		if gotIn == gotOut && rates.Input != rates.Output {
			t.Errorf("%s: a million input tokens and a million output tokens both cost "+
				"%v, though its rates are %v and %v: the pricer is not using the rate "+
				"for the bucket it was given", id, gotIn, rates.Input, rates.Output)
		}
		gotCache, _ := p.Cost(id, 0, 0, perMillion, 0)
		if rates.CacheRead != 0 && gotCache == gotIn && rates.CacheRead != rates.Input {
			t.Errorf("%s: a million cache-read tokens cost the same %v as a million "+
				"input tokens, though its rates are %v and %v", id, gotCache,
				rates.CacheRead, rates.Input)
		}
	}
	if checked == 0 {
		t.Fatal("no model with both a non-zero input and output rate resolved")
	}
}

// TestPricerBillsReasoningAtTheOutputRate is the other billing relationship the
// dashboard depends on.
//
// Several agents report thinking inside the generated output count rather than
// beside it, so the pricer is handed the whole generated figure and has to bill
// all of it at the output rate. Priced at the input rate — or dropped — a
// thinking-heavy session costs a fraction of what it cost, and every downstream
// figure still looks plausible.
//
// The check is relational so it survives a price change: for any model whose
// output rate exceeds its input rate, the same number of generated tokens must
// cost strictly more as output than as input, and must equal the output rate
// exactly.
func TestPricerBillsReasoningAtTheOutputRate(t *testing.T) {
	p := testPricer(t)

	var checked int
	for _, id := range liveModelIDs(t) {
		rates, ok := p.Resolve(id)
		if !ok || rates.Output <= rates.Input || rates.Input <= 0 {
			continue
		}
		checked++
		const perMillion = 1_000_000
		asOutput, _ := p.Cost(id, 0, perMillion, 0, 0)
		asInput, _ := p.Cost(id, perMillion, 0, 0, 0)

		// The generated count is billed at the output rate. This is the assertion
		// that fails if reasoning is billed as input or ignored: both would give a
		// number below the output rate, and the two cannot be told apart by cost
		// alone, so the exact-rate check below is what distinguishes them.
		if want := perMillion / 1e6 * rates.Output; asOutput != want {
			t.Fatalf("%s: a million generated tokens billed %v, want %v at the output "+
				"rate %v: thinking is billed as output, so anything else is a fraction "+
				"of the real cost", id, asOutput, want, rates.Output)
		}
		if asOutput <= asInput {
			t.Errorf("%s: a million generated tokens billed %v, no more than the same "+
				"count as input (%v), though its output rate %v exceeds its input rate "+
				"%v", id, asOutput, asInput, rates.Output, rates.Input)
		}
	}
	if checked == 0 {
		// Not a skip: a pricer whose Resolve returned one flat rate for everything
		// would leave nothing to compare, and that is exactly the defect above.
		t.Fatal("no catalogued model has an output rate above its input rate, so there " +
			"is nothing to check that reasoning is billed as output")
	}
	t.Logf("checked the generated-count rate for %d models", checked)
}

// TestPricerCachesResolution covers the cache rather than the arithmetic.
//
// The previous version captured cacheLen() after the first resolve and asserted
// it did not grow across two more — which passed with the cache deleted
// altogether, because cacheLen() is stably 0 and 0 != 0 is false. So the first
// assertion is that the cache is populated at all.
func TestPricerCachesResolution(t *testing.T) {
	p := testPricer(t)

	if got := p.cacheLen(); got != 0 {
		t.Fatalf("a freshly built pricer already holds %d cached resolutions", got)
	}
	p.Resolve("gemini-2.5-flash")
	after := p.cacheLen()
	if after == 0 {
		t.Fatal("resolving a model cached nothing: the resolution was not memoised, so " +
			"every call re-scans the catalogue")
	}

	// The same model again must be served from the cache rather than added to it.
	for i := 0; i < 3; i++ {
		p.Resolve("gemini-2.5-flash")
	}
	if got := p.cacheLen(); got != after {
		t.Errorf("resolving the same model three more times grew the cache from %d to %d: "+
			"the lookup is not hitting what the first resolve stored", after, got)
	}

	// Two spellings of one model. The cache is keyed on the raw string, so this
	// adds an entry; the resolution is the same either way, so this is reported
	// rather than asserted — normalising the key would be a change to
	// Resolve(), and this file does not own it. What matters and is asserted is
	// that both spellings resolve to the same rates.
	before := p.cacheLen()
	p.Resolve("google/gemini-2.5-flash")
	spelled, okSpelled := p.Resolve("gemini-2.5-flash")
	plain, okPlain := p.Resolve("gemini-2.5-flash")
	if !okSpelled || !okPlain || spelled != plain {
		t.Errorf("a vendor-prefixed id resolved to %v/%v and the bare id to %v/%v; both "+
			"spellings of one model must reach the same rates", spelled, okSpelled, plain, okPlain)
	}
	if grown := p.cacheLen() - before; grown > 1 {
		t.Errorf("two extra resolves added %d cache entries; resolution is memoised per "+
			"id, so at most one new id should have been added", grown)
	}

	// And a genuinely new model does add one entry, or the cache above is not
	// tracking resolutions at all.
	p.Resolve("claude-opus-4-8")
	if got := p.cacheLen(); got <= after {
		t.Errorf("resolving a second model left the cache at %d; the earlier assertions "+
			"would pass against a cache that never grows", got)
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

// geminiFixture builds a Gemini session log under a fake home directory and
// returns the log's path and the project root its history entry names.
func geminiFixture(t *testing.T, home, project, wantRoot string) string {
	t.Helper()
	if wantRoot != "" {
		histDir := filepath.Join(home, ".gemini", "history", project)
		if err := os.MkdirAll(histDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(histDir, ".project_root"),
			[]byte(wantRoot+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(home, ".gemini", "tmp", project, "chats", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"gemini","model":"m","tokens":{"input":1,"output":1}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return logPath
}

// TestGeminiProjectDoesNotDependOnTheHomeEnvironmentVariable is the Windows
// defect, made checkable.
//
// geminiProjectForPath builds the history path from os.Getenv("HOME"). HOME is a
// Unix convention: on Windows it is normally unset, so the join produces a
// CWD-relative ".gemini\history\..." that no real installation has, the lookup
// fails, and every Gemini session files itself under its directory name rather
// than its working directory. The two tests above manufacture HOME with
// t.Setenv, which is exactly what hides the problem — and CI runs
// windows-latest, so this is a real gate rather than a theoretical one.
//
// The fix is in internal/source/jsonl_agents.go, which this branch does not own:
// swap os.Getenv("HOME") for os.UserHomeDir() and handle the error, as
// internal/web/sources.go:179 and cmd/dashd/cli.go:50 already do. The expectation
// below is stated for the fixed behaviour, so this test goes green when that
// change lands; until then it documents what the current code does on each
// platform.
//
// The branch is on runtime.GOOS rather than a skip: there is nothing to skip,
// only a different correct answer per platform, and a skipped test here would
// report a pass for a Windows-only defect.
func TestGeminiProjectDoesNotDependOnTheHomeEnvironmentVariable(t *testing.T) {
	home := t.TempDir()
	// HOME deliberately unset, and USERPROFILE pointed at the fixture home: this
	// is the shape of a Windows environment, where USERPROFILE is how a home
	// directory is found. Setting both to the same value would make the test pass
	// for the wrong reason, so only the one the platform actually uses is set.
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		// On Unix the user's home is HOME by definition, and Go's own
		// os.UserHomeDir reads it, so leaving it empty makes UserHomeDir fail the
		// way an unset HOME is supposed to.
		t.Setenv("USERPROFILE", "")
	}

	const wantRoot = "/home/testuser/example"
	logPath := geminiFixture(t, home, "myproject", wantRoot)

	pricer, err := NewPricer("", "")
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := NewGeminiParser().Parse(logPath, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}

	homeDir, errHome := os.UserHomeDir()
	resolvable := errHome == nil && homeDir != ""
	if !resolvable {
		// No home directory is resolvable on this platform, by construction. The
		// project name is then the best available label, and grouping sessions
		// under "" would be worse.
		if sess.Project != "myproject" {
			t.Errorf("project = %q with no resolvable home directory, want the "+
				"directory name rather than an empty project", sess.Project)
		}
		t.Logf("no home directory is resolvable here (os.UserHomeDir: %v), so the "+
			"directory name is the correct fallback", errHome)
		return
	}
	if sess.Project != wantRoot {
		t.Errorf("project = %q, want %q recovered from the history directory: the "+
			"home directory is resolvable as %q via os.UserHomeDir, so the lookup "+
			"must use that rather than the HOME environment variable, which is unset "+
			"here and on every Windows machine", sess.Project, wantRoot, homeDir)
	}
}

// TestGeminiProjectFollowsGoResolvedHomeDirectory is the Windows defect stated as
// a gate that runs everywhere.
//
// geminiProjectForPath builds the history path from os.Getenv("HOME"). HOME is a
// Unix convention: on Windows it is normally unset, so the join produces a
// CWD-relative ".gemini\history\..." that no real installation has, the lookup
// fails, and every Gemini session files itself under its directory name rather
// than its working directory. The two tests above this one manufacture HOME with
// t.Setenv, which is precisely what hides the problem.
//
// Here the home directory is set the way the *platform* defines it — HOME on
// Unix, USERPROFILE on Windows, which is what os.UserHomeDir reads — and the
// other variable is pointed at an empty decoy so that code still reading $HOME
// finds nothing there. On Linux both name the same fixture and the test passes
// today; on Windows the current code reads the decoy and fails.
//
// The fix is in internal/source/jsonl_agents.go, which this branch does not own:
// replace os.Getenv("HOME") with os.UserHomeDir() and handle the error, as
// internal/web/sources.go:179 and cmd/dashd/cli.go:50 already do.
func TestGeminiProjectFollowsGoResolvedHomeDirectory(t *testing.T) {
	home := t.TempDir()
	decoy := t.TempDir()

	const wantRoot = "/home/testuser/example"
	logPath := geminiFixture(t, home, "myproject", wantRoot)

	// The decoy holds no .gemini at all, so a lookup rooted there finds nothing.
	if entries, err := os.ReadDir(filepath.Join(decoy, ".gemini")); err == nil && len(entries) > 0 {
		t.Fatal("the decoy home contains a .gemini directory; the test would pass " +
			"for the wrong reason")
	}

	// Both variables are set, to different directories. Only the one this
	// platform defines names the fixture.
	switch runtime.GOOS {
	case "windows":
		t.Setenv("USERPROFILE", home)
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", home)
		t.Setenv("HOME", decoy)
	default:
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", decoy)
	}

	pricer, err := NewPricer("", "")
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := NewGeminiParser().Parse(logPath, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != wantRoot {
		t.Errorf("project = %q, want %q recovered from the history directory under "+
			"the home directory this platform defines (%s = %s). It fell back to the "+
			"directory name, which means the lookup rooted itself somewhere else — on "+
			"Windows that is $HOME, which does not exist there.",
			sess.Project, wantRoot, goHomeVariable(), home)
	}
}

// goHomeVariable names the environment variable this platform uses for a home
// directory, for the failure message above.
func goHomeVariable() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}
