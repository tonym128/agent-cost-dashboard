package source

import "testing"

// A protobuf key varint carries a field number in its high bits and a wire type
// in its low three. Field numbers are at most 29 bits, but the key itself is a
// 32-bit value in the wire format, and nothing stops a corrupt or hostile blob
// from writing a larger one.
//
// Decoding the field number with `int32(key >> 3)` truncates rather than
// rejecting: a key for field 2^32+2 came back as Num: 2, and ProtoVarints gave
// map[2: 999999]. Field 2 is agyUsageInput, so a blob like that writes an
// arbitrary count into the input bucket of a step that has no such field — and
// because the wrapped number is a *valid, low* field number, it passes every
// plausibility check a reader applies before storing it.

// protoValue encodes a varint value, so a test blob carries the number it means
// rather than a hand-written approximation of it.
func protoValue(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

// protoKeyOf encodes a key varint for an arbitrary field number and wire type,
// without the narrowing a legal encoder would use.
func protoKeyOf(field uint64, wire uint64) []byte {
	key := field<<3 | wire
	var out []byte
	for {
		b := byte(key & 0x7f)
		key >>= 7
		if key != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if key == 0 {
			return out
		}
	}
}

// TestProtoRejectsOutOfRangeFieldNumbers covers the wrap itself and the boundary
// either side of it.
func TestProtoRejectsOutOfRangeFieldNumbers(t *testing.T) {
	cases := []struct {
		name      string
		field     uint64
		wantValid bool
	}{
		{"field 2", 2, true},
		{"field 28", 28, true},
		{"largest legal field", uint64(536870911), true},
		{"one past the largest legal field", uint64(536870911) + 1, false},
		{"field 2^32", 1 << 32, false},
		{"field 2^32+2 wraps onto field 2", 1<<32 + 2, false},
		{"field 2^40", 1 << 40, false},
		{"field 2^61", 1 << 61, false},
		{"field 0", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := append(protoKeyOf(tc.field, 0), protoValue(999999)...)
			got := ProtoVarints(data)
			if tc.wantValid {
				if v, ok := got[int32(tc.field)]; !ok || v != 999999 {
					t.Errorf("field %d: got %v, want {999999}; a legal field number must still decode",
						tc.field, got)
				}
				return
			}
			if len(got) != 0 {
				t.Errorf("field %d: got %v, want nothing", tc.field, got)
			}
			for num := range got {
				if num == 2 || num == 1 || num == int32(tc.field&0xffffffff) {
					t.Errorf("field %d was reported as field %d: the key truncated onto a real field", tc.field, num)
				}
			}
			if fl, ok := ProtoField(data, 2, 0); ok {
				t.Errorf("ProtoField found field 2 as %+v; a key of %d must not alias it",
					fl, tc.field)
			}
		})
	}
}

// TestProtoRejectsOutOfRangeFieldNumbersInAUsageBlob states the consequence in
// the terms the dashboard sees: an Antigravity step with no input field must not
// acquire an input count because of a corrupt key.
func TestProtoRejectsOutOfRangeFieldNumbersInAUsageBlob(t *testing.T) {
	usage := protoVarint(1, 1037) // a real model id
	usage = append(usage, protoVarint(3, 183)...)
	usage = append(usage, protoKeyOf(1<<32+2, 0)...) // an aliased input count
	usage = append(usage, protoValue(999999)...)     // ... of 999999

	got := ProtoVarints(usage)
	// The fields written before the bad key still decode — the scan stops at the
	// key, as it does for any other malformed construct — so the assertion is
	// that the aliased field is absent, not that the blob is empty.
	if _, present := got[agyUsageInput]; present {
		t.Errorf("usage blob reported field %d as %d; the out-of-range key aliased a real field",
			agyUsageInput, got[agyUsageInput])
	}
	if len(got) != 2 || got[1] != 1037 || got[3] != 183 {
		t.Errorf("usage blob decoded to %v, want exactly the two fields written before the bad key", got)
	}
	// The same blob with only the legal fields still reads, so the rejection is
	// about the key and not about the blob.
	if clean := ProtoVarints(append(protoVarint(1, 1037), protoVarint(3, 183)...)); len(clean) != 2 {
		t.Errorf("clean usage blob decoded to %v, want fields 1 and 3", clean)
	}
}

// FuzzProtoFieldNumberRange seeds the fuzzer with the keys that motivated the
// check, so a future change that re-widens the decode starts from the case that
// broke rather than from random bytes.
func FuzzProtoFieldNumberRange(f *testing.F) {
	// Legal, and must keep working.
	f.Add(protoKeyOf(1, 0))
	f.Add(protoKeyOf(2, 0))
	f.Add(protoKeyOf(28, 2))
	f.Add(protoKeyOf(uint64(536870911), 0))
	// Out of range, and must yield nothing.
	f.Add(protoKeyOf(0, 0))
	f.Add(protoKeyOf(uint64(536870911)+1, 0))
	f.Add(protoKeyOf(1<<32, 0))
	f.Add(protoKeyOf(1<<32+2, 0))
	f.Add(protoKeyOf(1<<40, 0))
	f.Add(protoKeyOf(1<<61, 0))
	// A whole ten-byte key at the varint limit.
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f})

	f.Fuzz(func(t *testing.T, key []byte) {
		// Prepend a varint value so a legal key has something to decode.
		data := append(append([]byte{}, key...), 0x2a)
		for _, fl := range ProtoDecode(data) {
			if fl.Num < 1 || fl.Num > 536870911 {
				t.Fatalf("field number %d is outside 1..%d", fl.Num, 536870911)
			}
		}
		for num := range ProtoVarints(data) {
			if num < 1 || num > 536870911 {
				t.Fatalf("ProtoVarints reported field number %d, outside 1..%d", num, 536870911)
			}
		}
	})
}
