package source

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// These are the numeric and timestamp coercions every JSONL parser funnels its
// fields through. They are the least-tested code in the package and the only
// place untrusted text becomes a token count, so the inputs below are the ones
// that would produce a plausible wrong number rather than an obvious failure:
// 1e400 (an infinity that JSON accepts as a literal but decoding cannot express),
// a string where a number was expected, and timestamps whose units disagree
// with the field they are in.

// TestToIntClampsHostileNumbers covers the clamping rule: a negative token count
// must never reach a total, whatever shape it arrives in.
func TestToIntClampsHostileNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want int64
	}{
		{"plain", float64(42), 42},
		{"zero", float64(0), 0},
		{"negative float", float64(-5), 0},
		{"fraction truncates", 42.9, 42},
		{"negative fraction", -0.5, 0},
		{"int64", int64(7), 7},
		{"negative int64", int64(-7), 0},
		{"int", 9, 9},
		{"negative int", -9, 0},
		{"json.Number", json.Number("1234"), 1234},
		{"json.Number negative", json.Number("-1234"), 0},
		// Below 2^53 a float can hold an exact integer; this is the boundary
		// where it cannot, so the truncation is documented rather than implied.
		{"large float", float64(1 << 60), 1 << 60},
		{"string", "42", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
		{"slice", []any{1}, 0},
		{"map", map[string]any{}, 0},
		// json.Number that does not parse: a malformed count in the log.
		{"json.Number garbage", json.Number("nope"), 0},
		{"json.Number empty", json.Number(""), 0},
		{"json.Number float text", json.Number("1.5"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInt(tc.in); got != tc.want {
				t.Errorf("toInt(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestNumOnNonFiniteNumbers covers the case the fuzz corpus names and no unit
// test did: a token count that is not finite.
//
// toInt itself converts a non-finite float with int64(), and Go's conversion of
// +Inf or NaN is implementation-defined — on this platform it yields
// math.MinInt64, which is negative. That is a latent defect (see the report):
// num() clamps negatives coming from the log but not ones produced by the
// conversion, and the fix belongs in jsonl.go, which this branch does not own.
//
// What *is* asserted here is the property that decides whether it can bite: a
// record that reaches the parser through encoding/json cannot carry a non-finite
// number at all. json.Unmarshal rejects 1e400 as out of range for float64, so
// the whole line is dropped before toInt sees it. Pinned here so that if the
// decoder is ever switched to json.Decoder.UseNumber — the change that would
// make the non-finite path reachable — this fails and says why.
func TestNumOnNonFiniteNumbers(t *testing.T) {
	// The literal a hostile log line would carry.
	var decoded map[string]any
	err := json.Unmarshal([]byte(`{"usage":{"input_tokens":1e400}}`), &decoded)
	if err == nil {
		t.Fatal("1e400 decoded as a float64 without error, so a non-finite token " +
			"count is reachable and toInt's conversion of it is a live defect")
	}
	t.Logf("1e400 is rejected at decode: %v", err)

	// With the number preserved verbatim — the shape json.Number arrives in — the
	// parser's own accessor does clamp, because json.Number.Float64 returns an
	// error for an out-of-range value.
	if got := num(map[string]any{"n": json.Number("1e400")}, "n"); got != 0 {
		t.Errorf("num(json.Number(\"1e400\")) = %d, want 0: an out-of-range number "+
			"is reported as unparseable, not as a count", got)
	}
}

// TestNumNeverReturnsNegative covers the caller: a negative value in the log must
// read as zero whatever type it arrives as.
func TestNumNeverReturnsNegative(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  map[string]any
		want int64
	}{
		{"absent", map[string]any{}, 0},
		{"nil value", map[string]any{"n": nil}, 0},
		{"negative", map[string]any{"n": float64(-1)}, 0},
		{"negative int64", map[string]any{"n": int64(-1)}, 0},
		{"string", map[string]any{"n": "100"}, 0},
		{"float", map[string]any{"n": float64(3.7)}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := num(tc.obj, "n"); got != tc.want {
				t.Errorf("num(%v, n) = %d, want %d", tc.obj, got, tc.want)
			}
		})
	}
}

// TestToFloatOnHostileInput: toFloat has no clamping, because a duration or a
// percentage is not a token count. What it must not do is panic or return NaN
// from a json.Number that does not parse, since NaN propagates into every
// arithmetic expression downstream.
func TestToFloatOnHostileInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want float64
	}{
		{"float", float64(1.5), 1.5},
		{"int64", int64(2), 2},
		{"int", 3, 3},
		{"json.Number", json.Number("4.25"), 4.25},
		{"json.Number garbage", json.Number("nope"), 0},
		{"json.Number empty", json.Number(""), 0},
		{"json.Number infinity", json.Number("1e400"), math.Inf(1)},
		{"string", "1.5", 0},
		{"nil", nil, 0},
		{"bool", false, 0},
		{"negative float", -2.5, -2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toFloat(tc.in)
			if math.IsNaN(got) {
				t.Fatalf("toFloat(%#v) = NaN, which propagates into every total "+
					"computed from it", tc.in)
			}
			if tc.name == "json.Number infinity" {
				// The one case that legitimately is not finite: an overflow in the
				// text itself. Recorded rather than silently zeroed, because
				// zeroing a duration would understate a response time.
				if !math.IsInf(got, 1) {
					t.Errorf("toFloat(%v) = %v, want +Inf", tc.in, got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("toFloat(%#v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseInt64 is the hand-rolled integer parser, previously 0% covered and
// reachable from parseTime for a timestamp written as a string.
//
// Its contract is strict on purpose: it is the fallback for a numeric field, so
// anything that is not a plain integer must be an error rather than a partial
// parse. A parser that returned 12 for "12abc" would turn a corrupt timestamp
// into a plausible 1970 date.
func TestParseInt64(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "7", want: 7},
		{in: "1779976545", want: 1779976545},
		{in: "1779976545123", want: 1779976545123},
		{in: "-1", want: -1},
		{in: "-0", want: 0},
		{in: "0000123", want: 123},
		{in: "", want: 0},
		// Everything below must be refused rather than partially parsed.
		{in: "nope", wantErr: true},
		{in: "12abc", wantErr: true},
		{in: "1.5", wantErr: true},
		{in: " 12", wantErr: true},
		{in: "12 ", wantErr: true},
		{in: "1e6", wantErr: true},
		{in: "0x10", wantErr: true},
		{in: "++1", wantErr: true},
		{in: "1-", wantErr: true},
		{in: "١٢٣", wantErr: true}, // Arabic-Indic digits
		{in: "1,000", wantErr: true},
	} {
		name := tc.in
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			got, err := parseInt64(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseInt64(%q) = %d with no error, want a refusal: a "+
						"partial parse becomes a plausible wrong timestamp", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseInt64(%q): %v", tc.in, err)
				return
			}
			if got != tc.want {
				t.Errorf("parseInt64(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseInt64RejectsOverflow covers the one case where the digit loop itself is
// wrong: a long enough string overflows int64 silently, wrapping to some other
// value. A wrapped epoch is still a plausible-looking date, which is worse than
// no date — 9223372036854775808 ms wraps to a negative instant before 1970, and
// a shorter overflow wraps to an arbitrary future one.
//
// KNOWN RED on this branch: the multiply-and-add loop has no bound check. The
// fix is in jsonl.go, which this branch does not own; see the report for the exact
// change. The expectation is stated for the fixed behaviour.
func TestParseInt64RejectsOverflow(t *testing.T) {
	for _, in := range []string{
		"9223372036854775808",  // MaxInt64 + 1
		"99999999999999999999", // well past it
		"18446744073709551616", // 2^64
	} {
		got, err := parseInt64(in)
		if err == nil {
			t.Errorf("parseInt64(%q) = %d with no error, want a refusal: int64 "+
				"overflow wraps silently, and a wrapped epoch is still a plausible "+
				"date", in, got)
		}
	}
	// The largest value that must still parse, so the guard above is not simply
	// refusing everything long.
	if got, err := parseInt64("9223372036854775807"); err != nil || got != math.MaxInt64 {
		t.Errorf("parseInt64(MaxInt64) = %d, %v; want it to parse", got, err)
	}
}

// TestParseTimeOnHostileInput covers the timestamp shapes these agents emit and
// the ones they should not.
//
// The unit question is the interesting one: OpenCode records epoch milliseconds
// while some logs record seconds under the same field name, so epochMillis
// widens small values. A parser that guessed wrong puts every call in 1970 or in
// the year 55000, and both render as a plausible date on the page.
func TestParseTimeOnHostileInput(t *testing.T) {
	const ms = int64(1779976545123)
	const sec = int64(1779976545)

	for _, tc := range []struct {
		name string
		in   any
		want time.Time
	}{
		{"rfc3339 nano", "2026-05-01T10:00:00.000000000Z", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)},
		{"rfc3339", "2026-05-01T10:00:00Z", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)},
		{"no zone", "2026-05-01T10:00:00.000", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)},
		{"space separated", "2026-05-01 10:00:00", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)},
		{"epoch millis as string", "1779976545123", time.UnixMilli(ms).UTC()},
		{"epoch millis as float", float64(ms), time.UnixMilli(ms).UTC()},
		{"epoch millis as int64", ms, time.UnixMilli(ms).UTC()},
		{"epoch millis as json.Number", json.Number("1779976545123"), time.UnixMilli(ms).UTC()},
		{"epoch seconds, widened", float64(sec), time.Unix(sec, 0).UTC()},
		{"epoch seconds as string", "1779976545", time.Unix(sec, 0).UTC()},

		// Everything below must read as "no timestamp": a call with no time is
		// stored outside every date window, which is honest, rather than at a
		// date the log never claimed.
		{"empty string", "", time.Time{}},
		{"garbage", "nope", time.Time{}},
		{"garbage with digits", "2026-05-01T99:99:99Z", time.Time{}},
		{"float string", "1.5", time.Time{}},
		{"nil", nil, time.Time{}},
		{"bool", true, time.Time{}},
		{"map", map[string]any{}, time.Time{}},
		{"zero millis", float64(0), time.Time{}},
		{"zero json.Number", json.Number("0"), time.Time{}},
		{"json.Number garbage", json.Number("nope"), time.Time{}},
		{"date only", "2026-05-01", time.Time{}},
		// A negative epoch is a real instant rather than a missing one, and the
		// store keeps it out of every date window via `ts > 0`. Zero is the value
		// that means "no timestamp", and it is asserted separately below.
		{"negative epoch", float64(-1), time.Unix(-1, 0).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTime(tc.in)
			if tc.want.IsZero() {
				if !got.IsZero() {
					t.Errorf("parseTime(%#v) = %s, want the zero time", tc.in, got.UTC())
				}
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("parseTime(%#v) = %s, want %s", tc.in, got.UTC(), tc.want.UTC())
			}
			if got.Location() != time.UTC {
				t.Errorf("parseTime(%#v) returned a %v, want UTC: a local zone here "+
					"makes the same log date differently on each machine", tc.in, got.Location())
			}
		})
	}
}

// TestEpochMillisDistinguishesSecondsFromMilliseconds is the widening rule, on its
// own because getting it wrong is silent: a second interpreted as a millisecond
// is January 1970, and a millisecond interpreted as a second is the year 55,000.
func TestEpochMillisDistinguishesSecondsFromMilliseconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int64
		want time.Time
	}{
		{"a second", 1779976545, time.Unix(1779976545, 0).UTC()},
		{"milliseconds", 1779976545123, time.UnixMilli(1779976545123).UTC()},
		{"one second exactly", 1, time.Unix(1, 0).UTC()},
		// The threshold is 1e11 ms ≈ 1973; below it a value is read as seconds.
		{"just below the threshold", 99999999999, time.Unix(99999999999, 0).UTC()},
		{"just above the threshold", 100000000001, time.UnixMilli(100000000001).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := epochMillis(tc.in); !got.Equal(tc.want) {
				t.Errorf("epochMillis(%d) = %s, want %s", tc.in, got.UTC(), tc.want.UTC())
			}
		})
	}

	// Zero is the zero time, not 1970: it means the log recorded nothing, and a
	// call dated 1970 would be counted in a window nobody is looking at.
	if got := epochMillis(0); !got.IsZero() {
		t.Errorf("epochMillis(0) = %s, want the zero time", got)
	}
	// A negative epoch is before the zero time, which Go represents as a real
	// instant; the important property is only that it is not mistaken for
	// "no timestamp" in the wrong direction, so it is asserted as-is.
	if got := epochMillis(-1); got.IsZero() {
		t.Error("epochMillis(-1) returned the zero time, which reads as no " +
			"timestamp recorded rather than as an instant before 1970")
	}
}

// TestAsMapAndAsSliceCoerceRatherThanFail covers the two accessors every parser
// uses to reach into a decoded record.
//
// A usage block that is null, a string where an object was expected, or a
// content array that is really a string are all shapes a corrupt log produces,
// and each has to read as empty rather than panic or as a wrong number.
func TestAsMapAndAsSliceCoerceRatherThanFail(t *testing.T) {
	t.Run("asMap", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   any
			// wantKeys is what the result must contain: a coercion either keeps
			// the object's own fields or produces an empty map, never a partial
			// one and never nil (which callers range over).
			wantKeys []string
		}{
			{"object", map[string]any{"a": 1}, []string{"a"}},
			{"null", nil, nil},
			{"string", "text", nil},
			{"slice", []any{1}, nil},
			{"number", 1.0, nil},
			{"bool", true, nil},
			{"empty object", map[string]any{}, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := asMap(tc.in)
				if got == nil {
					t.Fatal("asMap returned nil: callers range over it, and a nil " +
						"map renders as no data rather than as an empty object")
				}
				if len(got) != len(tc.wantKeys) {
					t.Errorf("asMap(%#v) = %v, want %d keys", tc.in, got, len(tc.wantKeys))
				}
				for _, k := range tc.wantKeys {
					if _, ok := got[k]; !ok {
						t.Errorf("asMap(%#v) dropped the field %q", tc.in, k)
					}
				}
			})
		}
	})

	t.Run("asSlice", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   any
			want int
		}{
			{"slice", []any{1, 2}, 2},
			{"empty slice", []any{}, 0},
			{"nil", nil, 0},
			{"object", map[string]any{}, 0},
			{"string", "text", 0},
			{"number", 1.0, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := len(asSlice(tc.in)); got != tc.want {
					t.Errorf("asSlice(%#v) has %d elements, want %d", tc.in, got, tc.want)
				}
			})
		}
	})

	t.Run("str", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			obj  map[string]any
			want string
		}{
			{"present", map[string]any{"k": "v"}, "v"},
			{"absent", map[string]any{}, ""},
			{"null", map[string]any{"k": nil}, ""},
			{"wrong type", map[string]any{"k": 42}, ""},
			{"object", map[string]any{"k": map[string]any{}}, ""},
			{"empty", map[string]any{"k": ""}, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := str(tc.obj, "k"); got != tc.want {
					t.Errorf("str(%v, k) = %q, want %q", tc.obj, got, tc.want)
				}
			})
		}
	})

	t.Run("boolOf", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   any
			want bool
		}{
			{"true", true, true},
			{"false", false, false},
			{"string true", "true", false},
			{"nil", nil, false},
			{"number", 1.0, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := boolOf(tc.in); got != tc.want {
					t.Errorf("boolOf(%#v) = %v, want %v", tc.in, got, tc.want)
				}
			})
		}
	})
}

// TestSpanReadsDurationsInEitherDirection covers the two timestamp fields a tool
// call carries.
//
// A tool result records a start and an end, and the two arrive in whatever order
// the log wrote them; a negative duration would render as a negative time in the
// table, and one longer than the session would be nonsense. Anything malformed
// is zero rather than a guess.
func TestSpanReadsDurationsInEitherDirection(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  map[string]any
		want float64
	}{
		{"forward", map[string]any{
			"startTime": "2026-05-01T10:00:00Z", "endTime": "2026-05-01T10:00:03Z",
		}, 3},
		// Reversed is the case worth naming: a log that wrote the end first.
		{"reversed is not negative", map[string]any{
			"startTime": "2026-05-01T10:00:03Z", "endTime": "2026-05-01T10:00:00Z",
		}, 0},
		{"same instant", map[string]any{
			"startTime": "2026-05-01T10:00:00Z", "endTime": "2026-05-01T10:00:00Z",
		}, 0},
		{"missing start", map[string]any{"endTime": "2026-05-01T10:00:03Z"}, 0},
		{"missing end", map[string]any{"startTime": "2026-05-01T10:00:00Z"}, 0},
		{"both missing", map[string]any{}, 0},
		{"unparseable", map[string]any{
			"startTime": "nope", "endTime": "2026-05-01T10:00:03Z",
		}, 0},
		{"epoch millis", map[string]any{
			"startTime": float64(1779976545000), "endTime": float64(1779976547000),
		}, 2},
		{"fractional", map[string]any{
			"startTime": "2026-05-01T10:00:00Z", "endTime": "2026-05-01T10:00:01.500Z",
		}, 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := span(tc.obj, "startTime", "endTime"); got != tc.want {
				t.Errorf("span = %v, want %v", got, tc.want)
			}
		})
	}
}
