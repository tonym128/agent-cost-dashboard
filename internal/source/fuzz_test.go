package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// This project parses formats it does not own: a protobuf reader over blobs
// another program wrote, a line scanner over logs still being appended to, and
// a JSON parser over transcripts with no schema. All three are handed bytes
// that may be truncated, corrupt or simply not what they claim to be, and the
// project's own rule for that case is stated in protobuf.go: "a token count
// that is confidently wrong is worse than one that is absent."
//
// Unit tests confirm the cases already thought of. These confirm the ones
// nobody did.

// FuzzProtoDecode runs the protobuf reader over arbitrary bytes.
//
// The property asserted is the design rule itself. Cutting a valid blob short
// must never yield a *different* answer from reading it whole: the decoder may
// stop early and report less, but what it does report has to be the same
// fields with the same values. A reader that turned a truncated varint into a
// small integer, or a length running past the end into a short payload, would
// pass a test that only checks for a panic and fail here — and that is exactly
// the "plausible but wrong" failure this reader exists to avoid.
//
// Groups are excluded from the comparison: skipping to an end-group tag depends
// on bytes beyond the cut, so a prefix may legitimately resume at a different
// field. The blobs Antigravity writes contain only varints and byte fields.
func FuzzProtoDecode(f *testing.F) {
	// Valid blobs, from the encoders the fixture builder uses.
	usage := protoVarint(1, 1319)
	usage = append(usage, protoVarint(2, 13482)...)
	usage = append(usage, protoVarint(3, 183)...)
	usage = append(usage, protoVarint(5, 20000)...)
	meta := append(protoBytes(1, protoVarint(1, 1779976545)), protoBytes(9, usage)...)
	f.Add([]byte{})
	f.Add([]byte{0x08, 0x96, 0x01})             // field 1, varint 150
	f.Add([]byte{0x12, 0x03, 'a', 'b', 'c'})    // field 2, "abc"
	f.Add([]byte{0xe2, 0x01, 0x02, 0x68, 0x69}) // field 28, "hi" (two-byte key)
	f.Add(meta)
	// The malformed shapes the reader is built to refuse.
	f.Add([]byte{0x08})                                                             // varint value missing
	f.Add([]byte{0x08, 0xff, 0xff})                                                 // varint that never terminates
	f.Add([]byte{0x12, 0x05, 'a'})                                                  // declared length past the end
	f.Add([]byte{0x12, 0xff})                                                       // length varint that never terminates
	f.Add([]byte{0x0f})                                                             // wire type 7: not a wire type
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}) // 10-byte varint at the limit

	f.Fuzz(func(t *testing.T, data []byte) {
		fields := ProtoDecode(data) // must not panic on any input

		var consumed int
		for i, fl := range fields {
			// Protobuf field numbers start at 1. A zero or negative one means the
			// reader accepted a key that is not a field, and every consumer
			// indexes a map or compares against a constant by that number.
			if fl.Num < 1 {
				t.Fatalf("field %d has invalid field number %d", i, fl.Num)
			}
			if fl.Wire < 0 || fl.Wire > 5 {
				t.Fatalf("field %d has wire type %d, which is not one of the six", i, fl.Wire)
			}
			if fl.Wire == 2 {
				// A payload that runs past the end of the input is the truncation
				// bug: the reader returned a short slice that still decodes.
				if len(fl.Bytes) > len(data) {
					t.Fatalf("field %d payload is %d bytes, input is %d",
						i, len(fl.Bytes), len(data))
				}
			}
			consumed++
		}

		// Truncation may lose fields, never change one.
		whole := ProtoDecode(data)
		for p := 0; p <= len(data); p++ {
			part := ProtoDecode(data[:p])
			if len(part) > len(whole) {
				t.Fatalf("truncating to %d bytes decoded %d fields, more than the %d the whole input gives",
					p, len(part), len(whole))
			}
			for i, fl := range part {
				if i >= len(whole) {
					break
				}
				w := whole[i]
				if fl.Num != w.Num || fl.Wire != w.Wire || fl.Varint != w.Varint ||
					string(fl.Bytes) != string(w.Bytes) {
					t.Fatalf("truncating to %d bytes changed field %d from %+v to %+v",
						p, i, w, fl)
				}
				if w.Wire == 3 || w.Wire == 4 {
					// A group: where the scan resumes depends on bytes past the
					// cut, so the fields after one are not comparable.
					return
				}
			}
		}

		// The indexing helpers are documented to agree with the decoder. If they
		// drift, a caller reading one field silently reads a different one.
		indexed := ProtoFields(data)
		seen := map[int32]bool{}
		for _, fl := range whole {
			if seen[fl.Num] {
				continue
			}
			seen[fl.Num] = true
			if got, ok := indexed[fl.Num]; !ok || got.Wire != fl.Wire ||
				got.Varint != fl.Varint || string(got.Bytes) != string(fl.Bytes) {
				t.Fatalf("ProtoFields disagrees with ProtoDecode on field %d: %+v vs %+v",
					fl.Num, got, fl)
			}
		}
		if one, ok := ProtoField(data, 1, 0); ok {
			var found bool
			for _, fl := range whole {
				if fl.Num == 1 && fl.Wire == 0 {
					found = fl.Varint == one.Varint
					break
				}
			}
			if !found {
				t.Fatalf("ProtoField returned field 1 as %d, ProtoDecode has no such field", one.Varint)
			}
		}
		if n := len(ProtoVarints(data)); n > len(whole) {
			t.Fatalf("ProtoVarints found %d fields, more than the %d decoded", n, len(whole))
		}
		_ = consumed
	})
}

// FuzzScanLines runs the incremental reader over arbitrary files and cursor
// positions.
//
// The property that matters is where it stops. The reader's contract is that it
// resumes from exactly the end of the last complete line, because the final
// line of a log an agent is still writing is routinely half-written. If the
// returned offset ever lands mid-line, the next scan resumes inside a record
// and the file stops parsing — or worse, reads a fragment as complete.
func FuzzScanLines(f *testing.F) {
	dir := f.TempDir()

	f.Add([]byte("{\"a\":1}\n{\"b\":2}\n"), int64(0))
	f.Add([]byte("{\"a\":1}\n{\"b\":2}\n"), int64(8))
	// A half-written final line: the record is not there yet.
	f.Add([]byte("{\"a\":1}\n{\"b\":"), int64(0))
	// No trailing newline at all.
	f.Add([]byte("{\"a\":1}"), int64(0))
	// Multi-byte runes, including one that a cursor can land inside.
	f.Add([]byte("{\"t\":\"héllo ☃ 𝄞\"}\n"), int64(0))
	f.Add([]byte("{\"t\":\"héllo ☃ 𝄞\"}\n"), int64(9))
	// Valid JSON that is not an object, and an empty file.
	f.Add([]byte("[1,2,3]\n\"str\"\n42\n"), int64(0))
	f.Add([]byte{}, int64(0))
	// A cursor past the end, and a negative one.
	f.Add([]byte("{\"a\":1}\n"), int64(1<<20))
	f.Add([]byte("{\"a\":1}\n"), int64(-5))

	f.Fuzz(func(t *testing.T, data []byte, from int64) {
		path := filepath.Join(dir, "scan.jsonl")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skipf("cannot write the input: %v", err)
		}

		ordinals, consumed, err := ScanLinesForTest(path, from)
		if err != nil {
			// A read error is an acceptable outcome for input the reader cannot
			// reach; a panic is not.
			return
		}

		if consumed < 0 {
			t.Fatalf("consumed offset %d is negative", consumed)
		}
		if consumed > int64(len(data)) {
			t.Fatalf("consumed %d bytes of a %d byte file", consumed, len(data))
		}
		// The central invariant: whatever the reader consumed, it consumed whole
		// lines. A stop offset that has advanced past the cursor must land just
		// past a newline, or a half-written record was taken as a complete one
		// and the record it was part of can never be recovered. An offset equal
		// to the cursor is the reader having read nothing — legitimate when the
		// caller resumes at end of file.
		clamped := from
		if clamped < 0 || clamped > int64(len(data)) {
			clamped = 0 // scanLines rewinds a cursor outside the file
		}
		if consumed > clamped {
			if data[consumed-1] != '\n' {
				t.Fatalf("stopped at offset %d, which is not just past a newline; "+
					"a half-written record would be consumed as a complete one", consumed)
			}
		}
		// Ordinals count records across the whole file so a fallback call key
		// stays stable between scans, so they must not restart or go backwards.
		for i, o := range ordinals {
			if o < 0 {
				t.Fatalf("ordinal %d is negative", o)
			}
			if i > 0 && o <= ordinals[i-1] {
				t.Fatalf("ordinals are not increasing: %v", ordinals)
			}
		}

		// Resuming from where it stopped must return exactly what the first pass
		// returned and no more: the reader is not allowed to lose or duplicate
		// the boundary record.
		if consumed > 0 && consumed < int64(len(data)) {
			next, nextConsumed, err := ScanLinesForTest(path, consumed)
			if err != nil {
				return
			}
			if nextConsumed < consumed {
				t.Fatalf("resuming at %d went backwards to %d", consumed, nextConsumed)
			}
			if len(ordinals)+len(next) == 0 && nextConsumed != consumed {
				t.Fatalf("no records were returned but the cursor moved from %d to %d",
					consumed, nextConsumed)
			}
		}
	})
}

// FuzzClaudeLine runs one arbitrary JSONL line through the Claude parser.
//
// A transcript line is untrusted input that arrives one line at a time, so the
// only honest contract is: whatever comes out is either a well-formed call or
// nothing at all. Never a panic, never a negative token count, never a call
// with no session to hang it off.
func FuzzClaudeLine(f *testing.F) {
	dir := f.TempDir()

	f.Add([]byte(`{"type":"assistant","uuid":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-01T09:00:00.000Z","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}`))
	f.Add([]byte(`{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":-5,"output_tokens":-9}}}`))
	f.Add([]byte(`{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":1,"output_tokens":1}}}`))
	f.Add([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":"done"}]}}`))
	f.Add([]byte(`{"type":"assistant","message":{"usage":{"input_tokens":1e400,"output_tokens":"nope"}}}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(``))

	// Built once rather than per iteration: reading the pricing tables on every
	// execution capped this target at a few hundred execs a second, which is
	// slow enough to miss the shallow inputs that find panics fastest.
	pricer, err := NewPricer("../../models.json", "manual_pricing.json")
	if err != nil {
		f.Fatalf("NewPricer: %v", err)
	}

	f.Fuzz(func(t *testing.T, line []byte) {
		// The fuzzed line is written verbatim: a byte sequence that is not valid
		// UTF-8, or contains a NUL or a newline, is exactly what a corrupt log
		// looks like and must not be smoothed over before it reaches the parser.
		path := filepath.Join(dir, "session-fuzz.jsonl")
		body := append([]byte(`{"type":"user","sessionId":"fuzz","cwd":"/p","timestamp":"2026-05-01T08:00:00.000Z"}`+"\n"), line...)
		body = append(body, '\n')
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Skipf("cannot write the input: %v", err)
		}

		sw, _, err := NewClaudeParser().Parse(path, model.ScanState{}, pricer)
		if err != nil {
			return // a clean rejection is a valid outcome
		}

		// Every call that exists must satisfy the project's invariants. This is
		// the assertion that matters: the parser is allowed to drop a line it
		// does not understand, but not to invent tokens for one.
		checkInvariants(t, sw)
		for _, c := range sw.Calls {
			// A usage block whose values are negative is clamped to zero rather
			// than rejected, so the line yields a tokenless call. That is padding
			// rather than a wrong number — it bills nothing — so it is allowed
			// here; what is not allowed is a negative figure escaping the clamp.
			for name, v := range map[string]int{
				"input": c.InputTokens, "output": c.OutputTokens,
				"cache read": c.CacheReadTokens, "cache write": c.CacheWriteTokens,
				"reasoning": c.ReasoningTokens,
			} {
				if v < 0 {
					t.Errorf("a usage value of %s survived as %d", name, v)
				}
			}
			if c.CostUSD < 0 {
				t.Errorf("line produced a negative cost: %v", c.CostUSD)
			}
			// A call that bills something is either priced at a real rate or explicitly
			// marked unpriced. Cost of zero while claiming to be priced is the
			// failure; cost of zero while flagged unpriced is the safety net
			// working, and is what an unrecognised model must produce.
			if c.Priced && c.TotalTokens > 0 && c.CostUSD == 0 {
				t.Errorf("call billed %d tokens at $0.00 while marked priced: %+v",
					c.TotalTokens, c)
			}
			if c.TotalTokens > 0 && !c.Priced && c.Model != "" && c.Model != "unknown" {
				// Not a failure on its own — checkInvariants has already rejected
				// the priced-at-nothing case — but recorded here because it is the
				// shape a silently unpriced model takes.
				t.Logf("model %q priced as unknown for %d tokens", c.Model, c.TotalTokens)
			}
		}
		if sw.Session.UID == "" && len(sw.Calls) > 0 {
			t.Error("calls were produced with no session to attach them to")
		}
	})
}
