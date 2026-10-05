package source

// A minimal protobuf reader, sufficient for the blobs Antigravity writes on its
// `steps.metadata` column and needing no external dependency.
//
// Every malformed-input case here stops the decode instead of returning a
// plausible-looking value. That is the whole design rule: a truncated varint
// that yields a partial integer becomes a token count on the dashboard, and a
// token count that is confidently wrong is worse than one that is absent.

// maxVarintBytes is protobuf's own limit for a 64-bit varint. Reading past it
// means a corrupt blob rather than a large number, and an unbounded shift would
// produce an arbitrary-precision integer.
const maxVarintBytes = 10

// PBField is one decoded protobuf field.
type PBField struct {
	Num  int32
	Wire int
	// Varint holds the value for wire type 0.
	Varint int64
	// Bytes holds the payload for wire type 2.
	Bytes []byte
}

// ProtoDecode decodes a message into its fields, in order.
//
// Deprecated proto2 groups (wire types 3 and 4) are legal protobuf and are
// skipped to their matching end tag rather than terminating the scan; returning
// at a group would silently discard every field after it.
func ProtoDecode(data []byte) []PBField {
	var out []PBField
	i, n := 0, len(data)
	for i < n {
		key, next, ok := readVarint(data, i, n)
		if !ok {
			return out
		}
		i = next
		fieldNum := int32(key >> 3)
		wire := int(key & 7)
		// Protobuf field numbers start at 1. A key decoding to 0 or a negative
		// number is not a field at all, and reporting one would index a map under
		// a key nothing can ask for, so the scan stops here as it does for any
		// other malformed construct.
		if fieldNum < 1 {
			return out
		}

		switch wire {
		case 0:
			v, next, ok := readVarint(data, i, n)
			if !ok {
				return out
			}
			i = next
			out = append(out, PBField{Num: fieldNum, Wire: 0, Varint: asInt64(v)})

		case 2:
			length, next, ok := readVarint(data, i, n)
			if !ok {
				return out
			}
			i = next
			// A length running past the end means truncation. Slicing anyway
			// would hand back a short payload that still decodes.
			if length > uint64(n-i) {
				return out
			}
			out = append(out, PBField{
				Num:   fieldNum,
				Wire:  2,
				Bytes: data[i : i+int(length)],
			})
			i += int(length)

		case 1:
			i += 8
		case 5:
			i += 4
		case 3:
			i = skipGroup(data, i, n)
		case 4:
			continue
		default:
			return out
		}
		if i < 0 {
			return out
		}
	}
	return out
}

// ProtoField returns the first field with the given number and wire type.
//
// The alternative — filtering ProtoDecode — materialises every field of a
// multi-kilobyte blob, copying each payload, to return one of them. This steps
// over the payloads it does not want.
func ProtoField(data []byte, fieldNum int32, wire int) (PBField, bool) {
	i, n := 0, len(data)
	for i < n {
		key, next, ok := readVarint(data, i, n)
		if !ok {
			return PBField{}, false
		}
		i = next
		fn := int32(key >> 3)
		wt := int(key & 7)
		if fn < 1 {
			return PBField{}, false
		}

		switch wt {
		case 2:
			length, next, ok := readVarint(data, i, n)
			if !ok {
				return PBField{}, false
			}
			i = next
			if length > uint64(n-i) {
				return PBField{}, false
			}
			if fn == fieldNum && wire == 2 {
				return PBField{Num: fn, Wire: 2, Bytes: data[i : i+int(length)]}, true
			}
			i += int(length)
		case 0:
			v, next, ok := readVarint(data, i, n)
			if !ok {
				return PBField{}, false
			}
			i = next
			if fn == fieldNum && wire == 0 {
				return PBField{Num: fn, Wire: 0, Varint: asInt64(v)}, true
			}
		case 1:
			i += 8
		case 5:
			i += 4
		case 3:
			i = skipGroup(data, i, n)
		default:
			return PBField{}, false
		}
	}
	return PBField{}, false
}

// ProtoFields indexes a decoded message by field number, keeping the first
// occurrence of each. Decoding once and looking up is what lets a single step's
// metadata serve the timestamp, the usage submessage and the tool descriptor
// without three separate scans of the same blob.
func ProtoFields(data []byte) map[int32]PBField {
	out := make(map[int32]PBField, 8)
	for _, f := range ProtoDecode(data) {
		if _, dup := out[f.Num]; !dup {
			out[f.Num] = f
		}
	}
	return out
}

// ProtoVarints collects a message's varint fields into a map.
func ProtoVarints(data []byte) map[int32]int64 {
	out := make(map[int32]int64, 8)
	for _, f := range ProtoDecode(data) {
		if f.Wire == 0 {
			if _, dup := out[f.Num]; !dup {
				out[f.Num] = f.Varint
			}
		}
	}
	return out
}

// ProtoString decodes a length-delimited field as UTF-8, returning "" when it is
// absent or not valid UTF-8 rather than substituting replacement characters
// that would then be displayed as mojibake.
func ProtoString(data []byte, fieldNum int32) string {
	f, ok := ProtoField(data, fieldNum, 2)
	if !ok || len(f.Bytes) == 0 {
		return ""
	}
	for _, b := range f.Bytes {
		// Fast path: a lone continuation or invalid lead byte means this is
		// binary, not a string.
		if b >= 0x80 && b < 0xC0 {
			return ""
		}
	}
	return string(f.Bytes)
}

// ProtoTimestamp decodes a google.protobuf.Timestamp ({1: seconds, 2: nanos}).
func ProtoTimestamp(data []byte) (int64, bool) {
	if len(data) == 0 {
		return 0, false
	}
	parts := ProtoVarints(data)
	secs, ok := parts[1]
	if !ok || secs == 0 {
		return 0, false
	}
	return secs + parts[2]/1e9, true
}

// readVarint reads one base-128 varint, reporting ok=false when it is truncated
// or longer than protobuf permits.
func readVarint(data []byte, i, n int) (uint64, int, bool) {
	var value uint64
	var shift uint
	for read := 0; read < maxVarintBytes; read++ {
		// i < 0 as well as i >= n: callers add decoded lengths to the cursor, so a
		// hostile one can leave it before the start of the slice, and an upper
		// bound alone does not catch that.
		if i < 0 || i >= n {
			return 0, i, false
		}
		b := data[i]
		i++
		value |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return value, i, true
		}
		shift += 7
	}
	return 0, i, false
}

// asInt64 reinterprets a varint as a signed two's-complement int64.
//
// Without this a negative counter — which some encoders write as a ten-byte
// varint — decodes to about 1.8e19 and passes any "clamp negatives to zero"
// guard as an enormous positive token count.
func asInt64(v uint64) int64 {
	return int64(v)
}

// skipGroup steps over a deprecated group body to its matching end-group tag.
func skipGroup(data []byte, i, n int) int {
	depth := 1
	for i < n && depth > 0 {
		key, next, ok := readVarint(data, i, n)
		if !ok {
			return n
		}
		i = next
		switch int(key & 7) {
		case 3:
			depth++
		case 4:
			depth--
		case 2:
			length, next, ok := readVarint(data, i, n)
			if !ok {
				return n
			}
			// The length has to be checked against what is left before it is used
			// as a position. Added blindly it is not a position at all: a length
			// above the maximum int made the cursor negative, and the loop
			// condition below — which only tests i < n — then let a negative index
			// through to data[i]. A length running past the end is truncation, so
			// treating it as one is also the right answer.
			if length > uint64(n-next) {
				return n
			}
			i = next + int(length)
		case 0:
			_, next, ok := readVarint(data, i, n)
			if !ok {
				return n
			}
			i = next
		case 1:
			i += 8
		case 5:
			i += 4
		default:
			return i
		}
	}
	return i
}
